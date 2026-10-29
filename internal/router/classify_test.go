package router

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bagOfWords is a test embedding: each word adds one to a hashed dimension.
func bagOfWords(text string) []float32 {
	v := make([]float32, 256)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return r < 'a' || r > 'z' }) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%256]++
	}
	return v
}

func fakeEmbed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = bagOfWords(t)
	}
	return out, nil
}

// waitReady waits for the index build.
func waitReady(t *testing.T, c *classifier) {
	t.Helper()
	for range 200 {
		if c.status().Ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the index was not built: %+v", c.status())
}

func TestStarterExamples(t *testing.T) {
	ex := StarterExamples()
	cells := map[string]int{}
	for _, e := range ex {
		cells[e.Task+"/"+e.Difficulty]++
	}
	for _, task := range Tasks[:len(Tasks)-1] {
		for _, d := range Difficulties[:len(Difficulties)-1] {
			if cells[task+"/"+d] < 5 {
				t.Errorf("%s/%s has %d starter examples", task, d, cells[task+"/"+d])
			}
		}
	}
}

func TestParseExamplesRefusesBadLines(t *testing.T) {
	for _, line := range []string{
		`{"text":"x","task":"unknown","difficulty":"easy"}`,
		`{"text":"x","task":"code","difficulty":"extreme"}`,
		`{"text":" ","task":"code","difficulty":"easy"}`,
		`{"text":"x","task":"code","difficulty":"easy","label":"y"}`,
		`{"text":"` + strings.Repeat("x", maxExampleText+1) + `","task":"code","difficulty":"easy"}`,
		`not json`,
	} {
		if _, err := ParseExamples(strings.NewReader("# comment\n\n" + line)); err == nil || !strings.Contains(err.Error(), "line 3") {
			t.Errorf("%.60s: %v", line, err)
		}
	}
}

func TestClassifierVotesOfNearestExamples(t *testing.T) {
	cfg := &Config{
		Models:   []LocalModel{{ID: "embed", Embedding: true}},
		Profiles: ProfilesFile{Classifier: ClassifierConfig{NoStarter: true, K: 3}},
		Examples: []Example{
			{"fix the null pointer bug in this go function", "code", "medium"},
			{"refactor this go function and fix the bug", "code", "medium"},
			{"write a go function that sorts a slice", "code", "easy"},
			{"prove that the square root of two is irrational", "math", "hard"},
			{"prove there are infinitely many primes", "math", "hard"},
		},
	}
	c := &classifier{embed: fakeEmbed, log: func(string, ...any) {}}
	c.update(cfg)
	// No profile tests a label, so nothing is built until a classification.
	if st := c.status(); st.Ready || st.Model != "embed" || st.Examples != 5 {
		t.Fatalf("status before use = %+v", st)
	}
	if got := c.classify(cfg.Profiles.Classifier, "fix this go bug"); got.Task != "unknown" || got.Error == "" {
		t.Errorf("before the build: %+v", got)
	}
	waitReady(t, c)

	got := c.classify(cfg.Profiles.Classifier, "please fix the bug in my go function")
	if got.Task != "code" || got.Difficulty != "medium" || len(got.Neighbors) != 3 || got.Error != "" {
		t.Errorf("code request: %+v", got)
	}
	got = c.classify(cfg.Profiles.Classifier, "prove that there are infinitely many twin primes")
	if got.Task != "math" || got.Difficulty != "hard" {
		t.Errorf("math request: %+v", got)
	}
	if got := c.classify(cfg.Profiles.Classifier, "  "); got.Task != "unknown" || got.Error == "" {
		t.Errorf("empty text: %+v", got)
	}

	// The same configuration keeps the index; new examples rebuild it.
	c.update(cfg)
	if !c.status().Ready {
		t.Error("an unchanged configuration dropped the index")
	}
	cfg.Examples = append(cfg.Examples, Example{"translate this to french", "writing", "easy"})
	c.update(cfg)
	if c.status().Ready {
		t.Error("new examples kept the old index")
	}
}

func TestClassifierErrorsGiveUnknown(t *testing.T) {
	var calls atomic.Int32
	failing := func(context.Context, string, []string) ([][]float32, error) {
		calls.Add(1)
		return nil, errors.New("llama-swap is down")
	}
	c := &classifier{embed: failing, log: func(string, ...any) {}}
	cfg := &Config{Models: []LocalModel{{ID: "embed", Embedding: true}}}
	c.update(cfg)
	for range 100 {
		if calls.Load() > 0 && c.status().Error != "" {
			break
		}
		c.classify(ClassifierConfig{}, "hello")
		time.Sleep(10 * time.Millisecond)
	}
	got := c.classify(ClassifierConfig{}, "hello")
	if got.Task != "unknown" || got.Difficulty != "unknown" || !strings.Contains(got.Error, "llama-swap is down") {
		t.Errorf("failed build: %+v", got)
	}

	// With no embedding model, the classifier says so and never embeds.
	c2 := &classifier{embed: failing, log: func(string, ...any) {}}
	c2.update(&Config{})
	if got := c2.classify(ClassifierConfig{}, "hello"); !strings.Contains(got.Error, "no embedding model") {
		t.Errorf("no model: %+v", got)
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("a", 1000) + strings.Repeat("b", 1000)
	got := clip(long)
	if !strings.HasPrefix(got, strings.Repeat("a", 800)+"\n...\n") || !strings.HasSuffix(got, strings.Repeat("b", 400)) {
		t.Errorf("clip kept %d runes", len([]rune(got)))
	}
	if clip("short") != "short" {
		t.Error("clip changed a short text")
	}
}

const labelProfiles = `classifier:
  k: 3
  no_starter: true
profiles:
  auto:
    local_model: small
    rules:
      - name: hard maths
        when: {task: [math], difficulty: [hard]}
        route: cloud/big
      - name: everything else
        route: local
`

func TestLabelRulesRouteAndLog(t *testing.T) {
	h := newHarness(t, labelProfiles)
	write(t, filepath.Join(h.dir, ExamplesName), `{"text":"prove there are infinitely many primes","task":"math","difficulty":"hard"}
{"text":"prove that the square root of two is irrational","task":"math","difficulty":"hard"}
{"text":"what is two plus two","task":"math","difficulty":"easy"}
{"text":"write a go function that sorts a slice","task":"code","difficulty":"easy"}
`)
	if err := h.router.Reload(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, h.router.classifier())

	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "prove that there are infinitely many primes of the form 4k+3"))
	_, _ = io.ReadAll(resp.Body)
	if r := resp.Header.Get("X-Sibyl-Route"); !strings.HasPrefix(r, "cloud/big") {
		t.Errorf("hard maths went to %q", r)
	}
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "write a go function that reverses a slice"))
	_, _ = io.ReadAll(resp.Body)
	if r := resp.Header.Get("X-Sibyl-Route"); !strings.HasPrefix(r, "local/small") {
		t.Errorf("easy code went to %q", r)
	}

	// The decision log keeps the labels but no text.
	var log string
	for range 100 {
		if b, err := os.ReadFile(filepath.Join(h.dir, "decisions.jsonl")); err == nil {
			log = string(b)
		}
		if strings.Count(log, "\n") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(log, `"task":"math","difficulty":"hard"`) || strings.Contains(log, "primes") {
		t.Errorf("decision log: %s", log)
	}

	// Explain shows the labels, the neighbours and the price of the route.
	resp = h.post(t, "/sibyl/explain", adminKey, map[string]any{"client": "laptop",
		"request": chatBody("auto", "prove that the square root of three is irrational")})
	var d Decision
	var raw map[string]json.RawMessage
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &raw)
	_ = json.Unmarshal(raw["classification"], &d.Classification)
	if d.Classification == nil || d.Classification.Task != "math" || len(d.Classification.Neighbors) != 3 {
		t.Errorf("explain: %s", b)
	}

	// /sibyl/classify needs the System API key.
	if resp := h.post(t, "/sibyl/classify", hybridKey, ClassifyRequest{Text: "x"}); resp.StatusCode != 401 {
		t.Errorf("a client key called classify: %d", resp.StatusCode)
	}
	resp = h.post(t, "/sibyl/classify", adminKey, ClassifyRequest{Text: "what is two plus three"})
	var cr ClassifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	if !cr.Index.Ready || cr.Index.Examples != 4 || cr.Classification.Task != "math" || cr.Classification.Difficulty != "easy" {
		t.Errorf("classify: %+v %+v", cr.Index, cr.Classification)
	}
}

func TestBadExamplesKeepLastConfig(t *testing.T) {
	h := newHarness(t, labelProfiles)
	write(t, filepath.Join(h.dir, ExamplesName), `{"text":"x","task":"poetry","difficulty":"easy"}`+"\n")
	if err := h.router.Reload(); err == nil || !strings.Contains(err.Error(), ExamplesName) {
		t.Errorf("bad examples: %v", err)
	}
}

func TestValidateClassifier(t *testing.T) {
	for _, c := range []ClassifierConfig{{K: -1}, {K: 51}, {TimeoutMS: -5}, {Model: "cloud/big"}} {
		if err := (ProfilesFile{Classifier: c}).Validate(ProvidersFile{}); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
	if err := (ProfilesFile{Classifier: ClassifierConfig{Model: "embed", K: 5, TimeoutMS: 500}}).Validate(ProvidersFile{}); err != nil {
		t.Error(err)
	}
}
