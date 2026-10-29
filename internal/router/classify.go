package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExamplesName is the owner's labelled examples, one JSON object per line.
const ExamplesName = "examples.jsonl"

// Limits for the examples.
const (
	maxExamples    = 5000
	maxExampleText = 4000
)

// The classifier defaults.
const (
	defaultK               = 7
	defaultClassifyTimeout = 300 * time.Millisecond
	// embedBatch is the number of texts in one embedding request when the
	// router builds the index.
	embedBatch = 32
)

// Example is one labelled example of the classifier.
type Example struct {
	Text       string `json:"text"`
	Task       string `json:"task"`
	Difficulty string `json:"difficulty"`
}

//go:embed starter.jsonl
var starterJSONL []byte

// StarterExamples returns the examples that ship with Sibyl.
func StarterExamples() []Example {
	ex, err := ParseExamples(bytes.NewReader(starterJSONL))
	if err != nil {
		panic("starter.jsonl: " + err.Error())
	}
	return ex
}

// ParseExamples reads examples, one JSON object per line. Blank lines and
// lines that start with # are skipped. Every example needs a text and a
// known task and difficulty other than unknown.
func ParseExamples(r io.Reader) ([]Example, error) {
	var out []Example
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var e Example
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if len(out) == maxExamples {
			return nil, fmt.Errorf("more than %d examples", maxExamples)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func (e Example) validate() error {
	switch {
	case strings.TrimSpace(e.Text) == "":
		return errors.New("text is empty")
	case len(e.Text) > maxExampleText:
		return fmt.Errorf("text is over %d bytes", maxExampleText)
	case e.Task == "unknown" || !slices.Contains(Tasks, e.Task):
		return fmt.Errorf("task %q: use one of %s", e.Task, strings.Join(Tasks[:len(Tasks)-1], ", "))
	case e.Difficulty == "unknown" || !slices.Contains(Difficulties, e.Difficulty):
		return fmt.Errorf("difficulty %q: use one of %s", e.Difficulty, strings.Join(Difficulties[:len(Difficulties)-1], ", "))
	}
	return nil
}

// ClassifierConfig is the classifier section of profiles.yaml. The router
// runs the classifier only for a profile with a task or difficulty rule.
type ClassifierConfig struct {
	// Model is the local embedding model. Empty means the first embedding
	// model in models.json.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// K is the number of nearest examples that vote. The default is 7.
	K int `yaml:"k,omitempty" json:"k,omitempty"`
	// TimeoutMS limits one classification. The default is 300.
	TimeoutMS int `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	// NoStarter leaves out the examples that ship with Sibyl, so only
	// examples.jsonl counts.
	NoStarter bool `yaml:"no_starter,omitempty" json:"no_starter,omitempty"`
}

func (c ClassifierConfig) validate() error {
	if c.K < 0 || c.K > 50 {
		return errors.New("classifier: k must be 0 to 50")
	}
	if c.TimeoutMS < 0 || c.TimeoutMS > 10000 {
		return errors.New("classifier: timeout_ms must be 0 to 10000")
	}
	if strings.ContainsAny(c.Model, " \t\r\n/:") {
		return fmt.Errorf("classifier: model %q is not a local model ID", c.Model)
	}
	return nil
}

func (c ClassifierConfig) k() int {
	if c.K > 0 {
		return c.K
	}
	return defaultK
}

func (c ClassifierConfig) timeout() time.Duration {
	if c.TimeoutMS > 0 {
		return time.Duration(c.TimeoutMS) * time.Millisecond
	}
	return defaultClassifyTimeout
}

// classifierModel returns the embedding model that the classifier uses.
func (c *Config) classifierModel() string {
	if m := c.Profiles.Classifier.Model; m != "" {
		return m
	}
	for _, m := range c.Models {
		if m.Embedding {
			return m.ID
		}
	}
	return ""
}

// examples returns the examples that the classifier uses.
func (c *Config) examples() []Example {
	var out []Example
	if !c.Profiles.Classifier.NoStarter {
		out = StarterExamples()
	}
	return append(out, c.Examples...)
}

// Neighbor is one of the nearest examples to a request.
type Neighbor struct {
	Text       string  `json:"text"`
	Task       string  `json:"task"`
	Difficulty string  `json:"difficulty"`
	Similarity float64 `json:"similarity"`
}

// Classification is the classifier's result for one request.
type Classification struct {
	Task       string     `json:"task"`
	Difficulty string     `json:"difficulty"`
	Neighbors  []Neighbor `json:"neighbors,omitempty"`
	Millis     int64      `json:"ms"`
	// Error is why the labels are unknown, if they are.
	Error string `json:"error,omitempty"`
}

// IndexStatus describes the example index.
type IndexStatus struct {
	Model    string `json:"model"`
	Examples int    `json:"examples"`
	Ready    bool   `json:"ready"`
	Error    string `json:"error,omitempty"`
}

// embedFunc embeds texts with a local model and returns one vector per
// text, in order.
type embedFunc func(ctx context.Context, model string, texts []string) ([][]float32, error)

// classifier holds the embedded examples. The router builds the index in
// the background, and retries until the embedding model answers. A build
// starts after a configuration change when a profile has a task or
// difficulty rule, else on the first classification.
type classifier struct {
	embed embedFunc
	log   func(msg string, args ...any)

	mu       sync.RWMutex
	sig      string // the model and examples that the index is for
	model    string
	examples []Example
	vecs     [][]float32 // unit length
	ready    bool
	err      error
	gen      int
	// pending is true when the index for sig is not built and no build runs.
	pending bool
}

// signature identifies a model and an example list.
func signature(model string, ex []Example) string {
	h := sha256.New()
	h.Write([]byte(model))
	for _, e := range ex {
		_, _ = fmt.Fprintf(h, "\x00%s\x00%s\x00%s", e.Text, e.Task, e.Difficulty)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// update starts a new index build when the model or the examples changed.
func (c *classifier) update(cfg *Config) {
	model, ex := cfg.classifierModel(), cfg.examples()
	sig := signature(model, ex)
	c.mu.Lock()
	if sig == c.sig {
		c.mu.Unlock()
		return
	}
	c.gen++
	c.sig, c.model, c.examples, c.vecs, c.ready, c.err = sig, model, ex, nil, false, nil
	c.pending = model != "" && len(ex) > 0
	if model == "" {
		c.err = errors.New("no embedding model: set classifier.model or install an embedding model")
	}
	c.mu.Unlock()
	if cfg.usesLabels() {
		c.start()
	}
}

// start builds the index in the background, unless it is built or a
// build runs.
func (c *classifier) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pending {
		return
	}
	c.pending = false
	go c.build(c.gen, c.model, c.examples)
}

// usesLabels reports whether a profile has a task or difficulty rule.
func (c *Config) usesLabels() bool {
	for _, p := range c.Profiles.Profiles {
		if p.usesLabels() {
			return true
		}
	}
	return false
}

// retryWaits are the waits between failed builds. The last one repeats.
var retryWaits = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second}

func (c *classifier) build(gen int, model string, ex []Example) {
	texts := make([]string, len(ex))
	for i, e := range ex {
		texts[i] = clip(e.Text)
	}
	for try := 0; ; try++ {
		vecs, err := c.embedAll(model, texts)
		c.mu.Lock()
		if c.gen != gen {
			c.mu.Unlock()
			return // a newer configuration replaced this build
		}
		if err == nil {
			c.vecs, c.ready, c.err = vecs, true, nil
			c.mu.Unlock()
			c.log("classifier index built", "model", model, "examples", len(ex))
			return
		}
		c.err = err
		c.mu.Unlock()
		if try == 0 {
			c.log("classifier index not built; retrying", "model", model, "err", err)
		}
		time.Sleep(retryWaits[min(try, len(retryWaits)-1)])
	}
}

func (c *classifier) embedAll(model string, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatch {
		batch := texts[i:min(i+embedBatch, len(texts))]
		// A cold embedding model can take a while to load.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		vecs, err := c.embed(ctx, model, batch)
		cancel()
		if err != nil {
			return nil, err
		}
		for _, v := range vecs {
			out = append(out, unit(v))
		}
	}
	return out, nil
}

// status describes the index.
func (c *classifier) status() IndexStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := IndexStatus{Model: c.model, Examples: len(c.examples), Ready: c.ready}
	if c.err != nil {
		st.Error = c.err.Error()
	}
	return st
}

// classify labels text with the votes of the k nearest examples, each
// weighted by its similarity. It never fails: an error gives unknown
// labels and says why.
func (c *classifier) classify(cc ClassifierConfig, text string) *Classification {
	start := time.Now()
	res := &Classification{Task: "unknown", Difficulty: "unknown"}
	defer func() { res.Millis = time.Since(start).Milliseconds() }()
	if strings.TrimSpace(text) == "" {
		res.Error = "the request has no user text"
		return res
	}
	c.start()
	c.mu.RLock()
	model, ex, vecs, ready, err := c.model, c.examples, c.vecs, c.ready, c.err
	c.mu.RUnlock()
	if !ready {
		res.Error = "the example index is not ready"
		if err != nil {
			res.Error += ": " + err.Error()
		}
		return res
	}
	ctx, cancel := context.WithTimeout(context.Background(), cc.timeout())
	defer cancel()
	q, err := c.embed(ctx, model, []string{clip(text)})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	query := unit(q[0])

	type scored struct {
		i   int
		sim float64
	}
	all := make([]scored, len(vecs))
	for i, v := range vecs {
		all[i] = scored{i, dot(query, v)}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].sim > all[b].sim })
	all = all[:min(cc.k(), len(all))]

	tasks, diffs := map[string]float64{}, map[string]float64{}
	for _, s := range all {
		e := ex[s.i]
		w := max(s.sim, 0)
		tasks[e.Task] += w
		diffs[e.Difficulty] += w
		res.Neighbors = append(res.Neighbors, Neighbor{Text: e.Text, Task: e.Task, Difficulty: e.Difficulty,
			Similarity: math.Round(s.sim*1000) / 1000})
	}
	res.Task, res.Difficulty = top(tasks, Tasks), top(diffs, Difficulties)
	return res
}

// top returns the label with the most votes. A tie goes to the label that
// comes first in order.
func top(votes map[string]float64, order []string) string {
	best, bestV := "unknown", 0.0
	for _, l := range order {
		if v := votes[l]; v > bestV {
			best, bestV = l, v
		}
	}
	return best
}

// clip keeps the start and the end of a long text. The instruction of a
// request is usually at one of them, and the embedding model's batch holds
// about 512 tokens.
func clip(s string) string {
	const head, tail = 800, 400
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	return string(r[:head]) + "\n...\n" + string(r[len(r)-tail:])
}

func unit(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	if n == 0 {
		return out
	}
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range min(len(a), len(b)) {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// embed asks llama-swap for embeddings.
func (s *Server) embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.LocalURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed with %s: %w", model, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed with %s: status %d: %s", model, resp.StatusCode, upstreamMessage(b))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("embed with %s: %w", model, err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embed with %s: a result has index %d", model, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embed with %s: no result for text %d", model, i)
		}
	}
	return vecs, nil
}

// loadExamples reads examples.jsonl in dir. A missing file has none.
func loadExamples(path string) ([]Example, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ex, err := ParseExamples(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ExamplesName, err)
	}
	return ex, nil
}
