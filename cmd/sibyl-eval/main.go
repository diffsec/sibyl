// Command sibyl-eval replays a labelled request set against a Sibyl box.
// It reports the classifier's accuracy, and the routes and estimated cost
// of each routing profile. With -send it also sends the requests and keeps
// the answers, and with -judge a model scores them.
//
// Without -send, nothing leaves the box: the classifier and explain run on
// the box, and explain does not send the request.
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Item is one labelled request of the set.
type Item struct {
	Prompt     string `json:"prompt"`
	Task       string `json:"task"`
	Difficulty string `json:"difficulty"`
	// OutputTokens is the expected answer length, for the cost estimate.
	OutputTokens int `json:"output_tokens,omitempty"`
}

const defaultOutputTokens = 500

type price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type explained struct {
	Route    string `json:"route"`
	Rule     string `json:"rule"`
	Price    *price `json:"price"`
	Features struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"features"`
}

type classified struct {
	Index struct {
		Ready    bool   `json:"ready"`
		Examples int    `json:"examples"`
		Model    string `json:"model"`
		Error    string `json:"error"`
	} `json:"index"`
	Classification struct {
		Task       string `json:"task"`
		Difficulty string `json:"difficulty"`
		Millis     int64  `json:"ms"`
		Error      string `json:"error"`
	} `json:"classification"`
}

// Result is one line of the -out file.
type Result struct {
	Profile      string  `json:"profile"`
	Prompt       string  `json:"prompt"`
	Task         string  `json:"task"`
	Difficulty   string  `json:"difficulty"`
	Route        string  `json:"route"`
	Answer       string  `json:"answer,omitempty"`
	PromptTokens int     `json:"prompt_tokens,omitempty"`
	Completion   int     `json:"completion_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Unpriced     bool    `json:"unpriced,omitempty"`
	LatencyMS    int64   `json:"latency_ms,omitempty"`
	// Scores are the judges' scores from 1 to 10, by judge model. A
	// judge that gave no number is left out.
	Scores map[string]float64 `json:"scores,omitempty"`
	Error  string             `json:"error,omitempty"`
}

type client struct {
	base, apiKey, inferenceKey string
	http                       *http.Client
	// out, when set, gets each result as it ends, so a long run keeps
	// what it did when it stops.
	out *json.Encoder
}

func main() {
	base := flag.String("url", os.Getenv("SIBYL_URL"), "the box's API address, such as https://192.168.1.26:8443")
	apiKeyFile := flag.String("api-key", "api.key", "file with the System API key")
	keyFile := flag.String("key", "inference.key", "file with an inference key, for -send")
	caFile := flag.String("ca", "", "file with the box's CA certificate, when the system does not trust it")
	set := flag.String("set", "tools/router-eval/requests.jsonl", "the labelled request set")
	profiles := flag.String("profiles", "auto", "comma-separated routing profiles to compare")
	who := flag.String("client", "default", "the router client whose policy explain uses")
	send := flag.Bool("send", false, "send the requests, which can cost money at providers")
	maxTokens := flag.Int("max-tokens", 8192, "with -send, the answer limit; reasoning models spend part of it thinking")
	judge := flag.String("judge", "", "with -send, comma-separated models that score each answer from 1 to 10")
	judgeTokens := flag.Int("judge-tokens", 4096, "the judge's limit; a reasoning judge thinks before the number")
	out := flag.String("out", "", "a JSON lines file for the results, written as each one ends")
	rescore := flag.String("rescore", "", "a results file from -out: add the -judge scores that it lacks, and write -out")
	retry := flag.Bool("retry", false, "with -rescore, send the requests that got no answer again, with -max-tokens")
	flag.Parse()

	o := options{who: *who, send: *send, maxTokens: *maxTokens, judgeTokens: *judgeTokens, retry: *retry}
	if *judge != "" {
		o.judges = strings.Split(*judge, ",")
	}
	var err error
	if *rescore != "" {
		err = rescoreFile(*base, *keyFile, *caFile, *rescore, *out, o)
	} else {
		err = run(*base, *apiKeyFile, *keyFile, *caFile, *set, *profiles, *out, o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sibyl-eval:", err)
		os.Exit(1)
	}
}

// options are the settings of one run.
type options struct {
	who         string
	send        bool
	maxTokens   int
	judges      []string
	judgeTokens int
	retry       bool
}

func run(base, apiKeyFile, keyFile, caFile, set, profileList, out string, o options) error {
	if base == "" {
		return errors.New("give the box's address with -url or SIBYL_URL")
	}
	if len(o.judges) > 0 && !o.send {
		return errors.New("-judge needs -send")
	}
	send := o.send
	items, err := readSet(set)
	if err != nil {
		return err
	}
	c := &client{base: strings.TrimSuffix(base, "/")}
	if c.apiKey, err = readKey(apiKeyFile); err != nil {
		return err
	}
	if send {
		if c.inferenceKey, err = readKey(keyFile); err != nil {
			return err
		}
	}
	if c.http, err = httpClient(caFile); err != nil {
		return err
	}
	profiles := strings.Split(profileList, ",")
	if out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		c.out = json.NewEncoder(f)
	}

	if err := c.reportClassifier(items); err != nil {
		return err
	}
	var results []Result
	for _, p := range profiles {
		rs, err := c.runProfile(strings.TrimSpace(p), items, o)
		if err != nil {
			return err
		}
		results = append(results, rs...)
	}
	reportProfiles(profiles, results, o)
	return nil
}

func readSet(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var items []Item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if it.Prompt == "" {
			return nil, fmt.Errorf("%s line %d: prompt is empty", path, n)
		}
		if it.OutputTokens <= 0 {
			it.OutputTokens = defaultOutputTokens
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s has no requests", path)
	}
	return items, sc.Err()
}

func readKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func httpClient(caFile string) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", caFile)
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Minute}, nil
}

// post sends a JSON body and decodes a JSON answer into v. It returns the
// response headers.
func (c *client) post(path, key string, body, v any) (http.Header, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.Header, fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.Header, json.Unmarshal(data, v)
}

func chat(model, prompt string, maxTokens int) map[string]any {
	return map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
	}
}

func profileModel(p string) string {
	if p == "auto" {
		return "auto"
	}
	return "auto:" + p
}

// reportClassifier prints the label accuracy of the classifier.
func (c *client) reportClassifier(items []Item) error {
	tasks, diffs := map[[2]string]int{}, map[[2]string]int{}
	var okTask, okDiff int
	var ms []int64
	for _, it := range items {
		var r classified
		if _, err := c.post("/system/router/classify", c.apiKey, map[string]string{"text": it.Prompt}, &r); err != nil {
			return err
		}
		if r.Classification.Error != "" {
			return fmt.Errorf("the classifier gave no labels: %s", r.Classification.Error)
		}
		got := r.Classification
		tasks[[2]string{it.Task, got.Task}]++
		diffs[[2]string{it.Difficulty, got.Difficulty}]++
		if got.Task == it.Task {
			okTask++
		}
		if got.Difficulty == it.Difficulty {
			okDiff++
		}
		ms = append(ms, got.Millis)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	n := len(items)
	fmt.Printf("Classifier: %d requests. Task right %d (%.0f%%). Difficulty right %d (%.0f%%). Time p50 %d ms, p95 %d ms.\n\n",
		n, okTask, pct(okTask, n), okDiff, pct(okDiff, n), ms[n/2], ms[min(n-1, n*95/100)])
	printConfusion("task", tasks)
	printConfusion("difficulty", diffs)
	return nil
}

func printConfusion(name string, m map[[2]string]int) {
	labels := map[string]bool{}
	for k := range m {
		labels[k[0]], labels[k[1]] = true, true
	}
	var ls []string
	for l := range labels {
		ls = append(ls, l)
	}
	sort.Strings(ls)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintf(w, "%s: labelled \\ classified\t%s\t\n", name, strings.Join(ls, "\t"))
	for _, want := range ls {
		row := []string{want}
		for _, got := range ls {
			row = append(row, strconv.Itoa(m[[2]string{want, got}]))
		}
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t")+"\t")
	}
	_ = w.Flush()
	fmt.Println()
}

func (c *client) runProfile(p string, items []Item, o options) ([]Result, error) {
	model := profileModel(p)
	var out []Result
	for _, it := range items {
		r := Result{Profile: p, Prompt: it.Prompt, Task: it.Task, Difficulty: it.Difficulty}
		var e explained
		if _, err := c.post("/system/router/explain", c.apiKey,
			map[string]any{"client": o.who, "request": chat(model, it.Prompt, it.OutputTokens)}, &e); err != nil {
			return nil, err
		}
		r.Route = e.Route
		local := strings.HasPrefix(e.Route, "local/")
		switch {
		case local:
		case e.Price == nil:
			r.Unpriced = true
		default:
			r.CostUSD = (float64(e.Features.PromptTokens)*e.Price.Input + float64(it.OutputTokens)*e.Price.Output) / 1e6
		}
		if o.send {
			c.send(&r, model, it.Prompt, o.maxTokens, e.Price)
			for _, j := range o.judges {
				if r.Error != "" {
					break
				}
				if v := c.score(j, it.Prompt, r.Answer, o.judgeTokens); v > 0 {
					if r.Scores == nil {
						r.Scores = map[string]float64{}
					}
					r.Scores[j] = v
				}
			}
			fmt.Fprintf(os.Stderr, "%s %d/%d %s %s %v\n", p, len(out)+1, len(items), r.Route, r.Error, r.Scores)
		}
		if c.out != nil {
			if err := c.out.Encode(r); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// send sends one request and fills the answer, the route that served it,
// the usage and the real cost.
func (c *client) send(r *Result, model, prompt string, maxTokens int, pr *price) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	start := time.Now()
	h, err := c.post("/v1/chat/completions", c.inferenceKey, chat(model, prompt, maxTokens), &resp)
	r.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
		return
	}
	if route := h.Get("X-Sibyl-Route"); route != "" {
		r.Route = strings.Fields(route)[0]
	}
	if len(resp.Choices) > 0 {
		r.Answer = resp.Choices[0].Message.Content
		if strings.TrimSpace(r.Answer) == "" {
			r.Error = "no answer: finish_reason " + resp.Choices[0].FinishReason
		}
	}
	r.PromptTokens, r.Completion = resp.Usage.Prompt, resp.Usage.Completion
	r.CostUSD, r.Unpriced = 0, false
	switch {
	case strings.HasPrefix(r.Route, "local/"):
	case pr == nil:
		r.Unpriced = true
	default:
		r.CostUSD = (float64(r.PromptTokens)*pr.Input + float64(r.Completion)*pr.Output) / 1e6
	}
}

var scoreRE = regexp.MustCompile(`\b(10|[1-9])\b`)

// score asks the judge model for a score from 1 to 10, or 0 when it gives
// none. The last number of the answer counts.
func (c *client) score(judge, prompt, answer string, limit int) float64 {
	q := "Score the answer to the request from 1 (useless) to 10 (as good as an expert's). " +
		"Judge correctness first, then completeness. Reply with the number only.\n\n<request>\n" + prompt +
		"\n</request>\n\n<answer>\n" + answer + "\n</answer>"
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if _, err := c.post("/v1/chat/completions", c.inferenceKey, chat(judge, q, limit), &resp); err != nil || len(resp.Choices) == 0 {
		return 0
	}
	all := scoreRE.FindAllString(resp.Choices[0].Message.Content, -1)
	if len(all) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(all[len(all)-1], 64)
	return v
}

func reportProfiles(profiles []string, results []Result, o options) {
	sent := o.send
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	head := "profile\trequests\tlocal\tprovider\tcost USD\tunpriced"
	if sent {
		head += "\terrors\tp50 latency ms"
	}
	for _, j := range o.judges {
		head += "\tscore by " + j
	}
	_, _ = fmt.Fprintln(w, head)
	for _, p := range profiles {
		p = strings.TrimSpace(p)
		var n, local, unpriced, errs int
		var cost float64
		score, scored := map[string]float64{}, map[string]int{}
		var lat []int64
		for _, r := range results {
			if r.Profile != p {
				continue
			}
			n++
			if strings.HasPrefix(r.Route, "local/") {
				local++
			}
			if r.Unpriced {
				unpriced++
			}
			cost += r.CostUSD
			if r.Error != "" {
				errs++
			}
			if r.LatencyMS > 0 {
				lat = append(lat, r.LatencyMS)
			}
			for j, v := range r.Scores {
				score[j] += v
				scored[j]++
			}
		}
		line := fmt.Sprintf("%s\t%d\t%d (%.0f%%)\t%d\t%.4f\t%d", p, n, local, pct(local, n), n-local, cost, unpriced)
		if sent {
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			p50 := int64(0)
			if len(lat) > 0 {
				p50 = lat[len(lat)/2]
			}
			line += fmt.Sprintf("\t%d\t%d", errs, p50)
		}
		for _, j := range o.judges {
			if scored[j] == 0 {
				line += "\t-"
				continue
			}
			line += fmt.Sprintf("\t%.2f (%d)", score[j]/float64(scored[j]), scored[j])
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_ = w.Flush()
	if sent {
		fmt.Println("\nCost uses the token counts that the box returned. Unpriced provider requests count as zero.")
	} else {
		fmt.Println("\nCost is an estimate: the router's prompt token estimate plus each request's output_tokens.")
		fmt.Println("Unpriced provider requests count as zero.")
	}

	routes := map[string]map[string]int{}
	for _, r := range results {
		if routes[r.Profile] == nil {
			routes[r.Profile] = map[string]int{}
		}
		routes[r.Profile][r.Route]++
	}
	fmt.Println("\nRoutes:")
	for _, p := range profiles {
		p = strings.TrimSpace(p)
		var keys []string
		for k := range routes[p] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s: %s %d\n", p, k, routes[p][k])
		}
	}
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return 100 * float64(a) / float64(n)
}

// rescoreFile reads the results of an earlier run and asks each judge for
// the scores that a result lacks, such as when the judge ran out of
// tokens. It writes every result to out and prints the profile report.
func rescoreFile(base, keyFile, caFile, in, out string, o options) error {
	switch {
	case base == "":
		return errors.New("give the box's address with -url or SIBYL_URL")
	case out == "" || out == in:
		return errors.New("-rescore needs an -out file other than its input")
	case len(o.judges) == 0:
		return errors.New("-rescore needs -judge")
	}
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var results []Result
	var profiles []string
	for n, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return fmt.Errorf("%s line %d: %w", in, n+1, err)
		}
		if !slices.Contains(profiles, r.Profile) {
			profiles = append(profiles, r.Profile)
		}
		results = append(results, r)
	}
	c := &client{base: strings.TrimSuffix(base, "/")}
	if c.inferenceKey, err = readKey(keyFile); err != nil {
		return err
	}
	if c.http, err = httpClient(caFile); err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	added, retried := 0, 0
	for i := range results {
		r := &results[i]
		if o.retry && r.Error != "" {
			// The price is not in the file, so a retried provider answer
			// counts as unpriced.
			again := Result{Profile: r.Profile, Prompt: r.Prompt, Task: r.Task, Difficulty: r.Difficulty, Route: r.Route}
			c.send(&again, profileModel(r.Profile), r.Prompt, o.maxTokens, nil)
			fmt.Fprintf(os.Stderr, "retried %s %.40q: %s\n", r.Profile, r.Prompt, again.Error)
			*r = again
			retried++
		}
		for _, j := range o.judges {
			if r.Error != "" || r.Scores[j] > 0 {
				continue
			}
			if v := c.score(j, r.Prompt, r.Answer, o.judgeTokens); v > 0 {
				if r.Scores == nil {
					r.Scores = map[string]float64{}
				}
				r.Scores[j] = v
				added++
			}
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "retried %d requests, added %d scores\n", retried, added)
	o.send = true
	reportProfiles(profiles, results, o)
	return nil
}
