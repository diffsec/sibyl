package tui

import (
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/client"
)

func TestLoadedText(t *testing.T) {
	if !strings.Contains(loadedText(nil), "does not answer") {
		t.Error("no llama-swap")
	}
	if !strings.Contains(loadedText(&client.Running{}), "none") {
		t.Error("nothing loaded")
	}
	r := &client.Running{
		Models:  []client.RunningModel{{ID: "big", State: "ready"}, {ID: "embed", State: "starting"}},
		Loading: []string{"embed", "small"},
		Errors:  map[string]string{"broken": "llama-swap /upstream/broken/health: 502 Bad Gateway"},
	}
	got := loadedText(r)
	for _, want := range []string{"big", "embed  starting", "small", "loading", "broken: the last load failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("loaded text lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "embed") != 1 {
		t.Errorf("a loading model that runs is listed twice:\n%s", got)
	}
	if r.State("small") != "loading" || r.State("big") != "ready" || r.State("gone") != "" {
		t.Error("Running.State")
	}
}
