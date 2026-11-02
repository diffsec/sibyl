package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

var ctrlS = tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}

// TestSaveKey checks that ctrl+s saves an edit form from its first field,
// and does nothing on a confirmation form.
func TestSaveKey(t *testing.T) {
	if ctrlS.String() != saveKey {
		t.Fatalf("ctrl+s reads as %q", ctrlS.String())
	}
	var mu sync.Mutex
	got := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got[r.Method+" "+r.URL.Path] = body
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Models: the settings form saves from its first field.
	m := newModelsView(c)
	m.list = []client.Model{{Model: models.Model{ID: "qwen"}, Settings: models.Settings{CtxSize: 4096}}}
	m.editForm(m.list[0])
	if m.help() != formHelp(true) {
		t.Errorf("help %q", m.help())
	}
	m.fields.CtxSize = "8192"
	for _, msg := range drain(m.update(ctrlS)) {
		m.update(msg)
	}
	mu.Lock()
	if b := got["PUT /models/qwen"]; b == nil || b["ctx_size"] != 8192.0 {
		t.Errorf("save body %v", b)
	}
	mu.Unlock()
	if m.form != nil {
		t.Error("the form stayed open")
	}

	// A delete form needs its answer.
	m.deleteForm(m.list[0])
	m.confirm = true
	m.update(ctrlS)
	if m.form == nil {
		t.Error("ctrl+s closed the delete form")
	}
	mu.Lock()
	if _, ok := got["DELETE /models/qwen"]; ok {
		t.Error("ctrl+s deleted the model")
	}
	mu.Unlock()
	if m.help() != formHelp(false) {
		t.Errorf("delete help %q", m.help())
	}

	// Router: the client form saves; the delete form does not.
	r := newRouterView(c)
	r.update(routerFixture())
	r.keyPress("n")
	r.name, r.policy = "laptop", "hybrid"
	for _, msg := range drain(r.update(ctrlS)) {
		r.update(msg)
	}
	mu.Lock()
	if b := got["POST /router/clients"]; b == nil || b["name"] != "laptop" {
		t.Errorf("router save body %v", b)
	}
	mu.Unlock()
	r.deleteForm("laptop")
	r.confirm = true
	r.update(ctrlS)
	if r.form == nil {
		t.Error("ctrl+s closed the router delete form")
	}
}
