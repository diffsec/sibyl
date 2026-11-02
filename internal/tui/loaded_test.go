package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
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

func TestLoadKeyToggles(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got = append(got, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newModelsView(c)
	v.list = []client.Model{{Model: models.Model{ID: "big"}}}
	l := tea.KeyPressMsg{Code: 'l', Text: "l"}
	drain(v.update(l))
	v.running = client.Running{Models: []client.RunningModel{{ID: "big", State: "ready"}}}
	drain(v.update(l))
	if strings.Join(got, " ") != "/models/big/load /models/big/unload" {
		t.Errorf("l on a model that is not loaded, then on a loaded one: %v", got)
	}
}
