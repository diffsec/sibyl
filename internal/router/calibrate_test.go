package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// calibrating makes the hybrid client calibrate, gives cloud/big a price
// and keeps every request.
func calibrating(t *testing.T, h *harness, calibration string) {
	t.Helper()
	write(t, filepath.Join(h.dir, ClientsName), fmt.Sprintf(`clients:
  - name: laptop
    key_sha256: %s
    policy: hybrid
    calibrate: true
  - name: homeauto
    key_sha256: %s
    policy: local-only
    calibrate: true
`, hash(hybridKey), hash(localKey)))
	b, err := os.ReadFile(filepath.Join(h.dir, ProvidersName))
	if err != nil {
		t.Fatal(err)
	}
	priced := strings.Replace(string(b), "models: [big]\n", "models: [big]\n    prices:\n      big: {input: 1, output: 2}\n", 1)
	write(t, filepath.Join(h.dir, ProvidersName), priced)
	b, err = os.ReadFile(filepath.Join(h.dir, ProfilesName))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.dir, ProfilesName), string(b)+calibration)
	// The stamp has a resolution of the file time, so a quick rewrite
	// can look unchanged. A new stamp forces the reload.
	h.router.stamp = ""
	if err := h.router.Reload(); err != nil {
		t.Fatal(err)
	}
	h.router.CalibrationDir = filepath.Join(h.dir, "calibration")
}

const keepAll = "calibration:\n  sample_rate: 1\n"

// samplesAfter waits for the handler to write its sample. The handler
// writes it after the client has the answer.
func samplesAfter(t *testing.T, h *harness, want int) []Sample {
	t.Helper()
	if want == 0 {
		// Nothing to wait for: give a handler time to write a wrong one.
		time.Sleep(100 * time.Millisecond)
		return LoadSamples(h.router.CalibrationDir)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := LoadSamples(h.router.CalibrationDir)
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitIndex waits for the classifier's example index.
func waitIndex(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !h.router.classifier().status().Ready {
		if time.Now().After(deadline) {
			t.Fatalf("index not ready: %+v", h.router.classifier().status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSampling(t *testing.T) {
	h := newHarness(t, testProfiles)
	calibrating(t, h, keepAll)
	waitIndex(t, h)
	stream := chatBody("auto", "write a haiku")
	stream["stream"] = true
	for _, c := range []struct {
		name string
		key  string
		body map[string]any
		keep bool
	}{
		{"plain request", hybridKey, chatBody("auto", "write a haiku"), true},
		{"streamed request", hybridKey, stream, true},
		{"sensitive request", hybridKey, chatBody("auto", "deploy project-x"), false},
		{"local-only client", localKey, chatBody("auto", "write a haiku"), false},
		{"model named, no profile", hybridKey, chatBody("small", "write a haiku"), false},
		{"default client does not calibrate", defaultKey, chatBody("auto", "write a haiku"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ClearSamples(h.router.CalibrationDir); err != nil {
				t.Fatal(err)
			}
			resp := h.post(t, "/v1/chat/completions", c.key, c.body)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			_, _ = io.ReadAll(resp.Body)
			got := samplesAfter(t, h, map[bool]int{true: 1}[c.keep])
			if (len(got) == 1) != c.keep {
				t.Fatalf("kept %d samples, want keep %v", len(got), c.keep)
			}
			if !c.keep {
				return
			}
			smp := got[0]
			if smp.Served != WinLocal || smp.Local == nil || smp.Provider != nil || smp.LocalModel != "small" {
				t.Fatalf("sample %+v", smp)
			}
			want := "from local"
			if c.body["stream"] == true {
				want = "Hello"
			}
			if smp.Local.Answer != want || smp.Client != "laptop" || smp.Profile != "auto" {
				t.Errorf("answer %q client %q profile %q", smp.Local.Answer, smp.Client, smp.Profile)
			}
			if smp.Task == "" || smp.Difficulty == "" {
				t.Errorf("labels %q %q, want the classifier's", smp.Task, smp.Difficulty)
			}
		})
	}
}

func TestSamplingRateZeroKeepsNothing(t *testing.T) {
	h := newHarness(t, testProfiles)
	calibrating(t, h, "calibration:\n  sample_rate: 0\n")
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	_, _ = io.ReadAll(resp.Body)
	if n := len(samplesAfter(t, h, 0)); n != 0 {
		t.Errorf("kept %d samples", n)
	}
}

// runWorker runs the worker steps until it stops.
func runWorker(h *harness) {
	for h.router.calibrateOne(context.Background()) {
	}
}

func TestCalibrateCompareAndJudge(t *testing.T) {
	for _, verdict := range []string{`{"winner": "A"}`, `{"winner": "B"}`, `thinking... {"winner": "tie"}`} {
		t.Run(verdict, func(t *testing.T) {
			h := newHarness(t, testProfiles)
			calibrating(t, h, keepAll)
			h.provider.verdict = verdict
			resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "write a haiku"))
			_, _ = io.ReadAll(resp.Body)
			samplesAfter(t, h, 1)
			runWorker(h)
			got := LoadSamples(h.router.CalibrationDir)
			if len(got) != 1 {
				t.Fatalf("%d samples", len(got))
			}
			smp := got[0]
			if smp.Provider == nil || smp.Provider.Route != "cloud/big" || smp.Provider.Answer != "from cloud" {
				t.Fatalf("provider side %+v: %s %+v", smp.Provider, smp.LastError, LoadStatus(h.router.CalibrationDir))
			}
			v := smp.Verdict
			if v == nil {
				t.Fatalf("no verdict: %s", smp.LastError)
			}
			if v.Judge != "cloud/big" || !v.JudgeIsSide {
				t.Errorf("judge %q is side %v", v.Judge, v.JudgeIsSide)
			}
			want := WinTie
			switch verdict {
			case `{"winner": "A"}`:
				want = map[bool]string{true: WinLocal, false: WinProvider}[v.LocalFirst]
			case `{"winner": "B"}`:
				want = map[bool]string{true: WinProvider, false: WinLocal}[v.LocalFirst]
			}
			if v.Winner != want || v.LocalOK != (want != WinProvider) {
				t.Errorf("winner %q local_ok %v, want %q", v.Winner, v.LocalOK, want)
			}
			// The compare request does not stream, and the judge does not see
			// the model names.
			h.provider.mu.Lock()
			reqs := append([]map[string]any(nil), h.provider.got...)
			h.provider.mu.Unlock()
			if len(reqs) != 2 || reqs[0]["stream"] != false {
				t.Fatalf("provider requests %v, want compare then judge", reqs)
			}
			if b, _ := json.Marshal(reqs[1]["messages"]); strings.Contains(string(b), "local/small") || strings.Contains(string(b), "cloud/big") {
				t.Errorf("the judge prompt names a model: %s", b)
			}
			if day, _ := h.router.Spend.Spent(CalibrationSpender, time.Now()); day <= 0 {
				t.Errorf("calibration spend %v, want more than 0", day)
			}
			if day, _ := h.router.Spend.Spent("laptop", time.Now()); day != 0 {
				t.Errorf("client spend %v, want 0", day)
			}
			r := Summarize(got)
			if r.Judged != 1 || len(r.Cells) != 1 || r.Cells[0].Comparisons != 1 {
				t.Errorf("report %+v", r)
			}
		})
	}
}

func TestCalibrateWaits(t *testing.T) {
	for _, c := range []struct {
		name, calibration, wantReason string
		unpriced, busy                bool
	}{
		{"budget zero", "  daily_usd: 0\n", "daily_usd is 0", false, false},
		{"unpriced provider", "", "has no price", true, false},
		{"local request in flight", "", "in flight", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testProfiles)
			calibrating(t, h, keepAll+c.calibration)
			if c.unpriced {
				write(t, filepath.Join(h.dir, ProvidersName), fmt.Sprintf("providers:\n  cloud:\n    type: openai\n    base_url: %s\n    models: [big]\n  claude:\n    type: anthropic\n    base_url: https://example.com\n",
					h.router.config().Providers.Providers["cloud"].BaseURL))
				h.router.stamp = ""
				if err := h.router.Reload(); err != nil {
					t.Fatal(err)
				}
			}
			h.provider.verdict = `{"winner": "A"}`
			resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "write a haiku"))
			_, _ = io.ReadAll(resp.Body)
			samplesAfter(t, h, 1)
			if c.busy {
				h.router.localInflight.Add(1)
			}
			runWorker(h)
			st := LoadStatus(h.router.CalibrationDir)
			if st == nil || st.State != "waiting" || !strings.Contains(st.Reason, c.wantReason) {
				t.Fatalf("status %+v, want waiting for %q", st, c.wantReason)
			}
			if smp := LoadSamples(h.router.CalibrationDir)[0]; smp.Provider != nil || smp.Attempts != 0 {
				t.Errorf("sample %+v, want untouched", smp)
			}
		})
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	s := &Server{CalibrationDir: dir}
	now := time.Now()
	for i, age := range []time.Duration{20 * 24 * time.Hour, 3 * time.Hour, 2 * time.Hour, time.Hour} {
		if err := s.writeSample(Sample{ID: sampleID(now.Add(-age)), Profile: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	s.prune(CalibrationConfig{MaxSamples: 2}, now)
	got := LoadSamples(dir)
	if len(got) != 2 || got[0].Profile != "2" || got[1].Profile != "3" {
		t.Errorf("kept %+v, want the two newest", got)
	}
}

func TestCalibrationValidate(t *testing.T) {
	providers := ProvidersFile{Providers: map[string]Provider{"cloud": {Type: ProviderOpenAI, BaseURL: "https://x"}}}
	for _, c := range []struct {
		cfg CalibrationConfig
		ok  bool
	}{
		{CalibrationConfig{}, true},
		{CalibrationConfig{SampleRate: ptr(1.5)}, false},
		{CalibrationConfig{DailyUSD: ptr(-1.0)}, false},
		{CalibrationConfig{Judge: "cloud/big", Compare: "cloud/big"}, true},
		{CalibrationConfig{Judge: "local"}, true},
		{CalibrationConfig{Compare: "local"}, false},
		{CalibrationConfig{Judge: "nobody/big"}, false},
	} {
		if err := c.cfg.validate(providers); (err == nil) != c.ok {
			t.Errorf("%+v: err %v, want ok %v", c.cfg, err, c.ok)
		}
	}
}

func judgedSample(t time.Time, text string, ok bool) Sample {
	b, _ := json.Marshal(chatBody("auto", text))
	w := WinLocal
	if !ok {
		w = WinProvider
	}
	return Sample{ID: sampleID(t), LocalModel: "small", Request: b, Verdict: &Verdict{Winner: w, LocalOK: ok}}
}

func TestNeedsProvider(t *testing.T) {
	h := newHarness(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: calibration says provider
        when: {needs_provider: true}
        route: cloud/big
      - name: everything else
        route: local
`)
	calibrating(t, h, keepAll)
	waitIndex(t, h)
	route := func(text string) (string, map[string]any) {
		resp := h.post(t, "/sibyl/explain", adminKey, map[string]any{"client": "laptop", "request": chatBody("auto", text)})
		var d map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return fmt.Sprint(d["route"]), d
	}
	now := time.Now()
	var samples []Sample
	for i := range MinOutcomes - 1 {
		samples = append(samples, judgedSample(now.Add(time.Duration(i)), "prove the theorem about prime gaps", false))
	}
	h.router.classifier().syncOutcomes(samples)
	if got, d := route("prove the theorem about prime gaps"); got != "local/small" {
		t.Fatalf("with %d outcomes the route is %s, want local while the vote is unknown: %v", len(samples), got, d)
	}
	for i := range 5 {
		samples = append(samples, judgedSample(now.Add(time.Hour+time.Duration(i)), "say hello to my friend", true))
	}
	h.router.classifier().syncOutcomes(samples)
	if got, d := route("prove the theorem about prime gaps"); got != "cloud/big" {
		t.Errorf("hard request: route %s, want cloud/big: %v", got, d)
	}
	if got, d := route("say hello to my friend"); got != "local/small" {
		t.Errorf("easy request: route %s, want local/small: %v", got, d)
	}
	// Deleted samples stop voting.
	h.router.classifier().syncOutcomes(nil)
	if got, _ := route("prove the theorem about prime gaps"); got != "local/small" {
		t.Errorf("after the samples went: route %s, want local/small", got)
	}
}
