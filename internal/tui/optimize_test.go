package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/optimize"
)

func doneRun(verified bool, backend string) *client.Optimize {
	return &client.Optimize{Status: "done", Profile: optimize.Single, Recommendation: &optimize.Recommendation{
		Baseline: optimize.Result{Candidate: optimize.Candidate{Backend: "hip"}, GenTPS: 40, PromptTPS: 900},
		Best:     optimize.Result{Candidate: optimize.Candidate{Backend: "vulkan", UBatch: 1024}, GenTPS: 50, PromptTPS: 1300},
		Backend:  backend, Verified: verified,
		Changes: []optimize.Change{{Setting: "ubatch", From: "default", To: "1024"}},
	}}
}

func TestOptimizeText(t *testing.T) {
	for _, c := range []struct {
		o    *client.Optimize
		want []string
	}{
		{&client.Optimize{Status: "running", Profile: optimize.Single, Stage: "batch",
			Results: []optimize.Result{{GenTPS: 1}, {Error: "oom"}}}, []string{"running", "stage batch", "1 settings measured", "o cancels"}},
		{&client.Optimize{Status: "failed", Error: "the baseline measurement failed"}, []string{"failed the baseline"}},
		{doneRun(true, "vulkan"), []string{" 40.0 tok/s generation", " 50.0 tok/s generation", "ubatch           default → 1024", "a applies"}},
		{doneRun(false, ""), []string{"did not load"}},
	} {
		got := ansi.Strip(optimizeText(c.o))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("no %q in\n%s", w, got)
			}
		}
	}
	if optimizeText(nil) != "" {
		t.Error("text for no run")
	}
}

func TestOptimizeForms(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.Method != http.MethodGet {
			got = append(got, r.Method+" "+r.URL.Path+" "+toJSON(b))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newModelsView(c)
	v.advisors = []string{"cloud/big"}
	m := client.Model{}
	m.ID = "big"

	// Start a throughput run.
	v.optimizeForm(m)
	if v.action != "optimize" || v.saveable() {
		t.Fatalf("action %q saveable %v", v.action, v.saveable())
	}
	v.optProfile, v.optAdvisor, v.confirm, v.form = string(optimize.Throughput), "cloud/big", true, nil
	drain(v.submit())
	// A running run offers to cancel.
	m.Optimize = &client.Optimize{Status: "running"}
	v.optimizeForm(m)
	if v.action != "cancel-optimize" {
		t.Fatalf("action %q", v.action)
	}
	v.confirm, v.form = true, nil
	drain(v.submit())
	// Apply with the backend switch.
	m.Optimize = doneRun(true, "vulkan")
	v.applyForm(m)
	if v.action != "apply-optimize" {
		t.Fatalf("action %q", v.action)
	}
	v.confirm, v.optBackend, v.form = true, true, nil
	drain(v.submit())
	// esc leaves no action behind, so the settings form saves again.
	v.optimizeForm(m)
	v.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.action != "" {
		t.Errorf("esc left action %q", v.action)
	}
	// An unverified run and no run do not open a form.
	m.Optimize = doneRun(false, "")
	if v.applyForm(m); v.action != "" {
		t.Error("an unverified run opened the apply form")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{`POST /models/big/optimize {"advisor":{"model":"big","provider":"cloud"},"profile":"throughput"}`, `DELETE /models/big/optimize null`,
		`POST /models/big/optimize/apply {"backend":true}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s", strings.Join(got, "\n"))
	}
}
