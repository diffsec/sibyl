package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
)

func TestSelectTextMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := New(c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if v := a.View(); v.MouseMode != tea.MouseModeCellMotion || !strings.Contains(v.Content, "[ctrl+t] select text") {
		t.Fatalf("the mouse is not on at the start: mode %v", v.MouseMode)
	}
	ctrlT := tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}
	a.Update(ctrlT)
	if v := a.View(); v.MouseMode != tea.MouseModeNone || !strings.Contains(v.Content, "[ctrl+t] mouse on") {
		t.Errorf("ctrl+t did not give the mouse back: mode %v", v.MouseMode)
	}
	a.Update(ctrlT)
	if v := a.View(); v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("a second ctrl+t did not take the mouse again: mode %v", v.MouseMode)
	}

	// A new client key turns the mouse off until it is hidden.
	a.active = 5
	r := a.views[5].(*routerView)
	r.key, r.keyFor = "sib-0123", "laptop"
	v := a.View()
	if v.MouseMode != tea.MouseModeNone {
		t.Errorf("the mouse is on while the key shows: mode %v", v.MouseMode)
	}
	out := ansiRE.ReplaceAllString(v.Content, "")
	if !strings.Contains(out, "\nsib-0123\n") || !strings.Contains(out, "[y] copy the key") || strings.Contains(out, "ctrl+t") {
		t.Errorf("key screen:\n%s", out)
	}
	a.Update(ctrlT)
	if a.View().MouseMode != tea.MouseModeNone {
		t.Error("ctrl+t took the mouse while the key shows")
	}
	var copied bool
	for _, m := range drain(r.keyPress("y")) {
		if n, ok := m.(noteMsg); ok && n.ok && strings.Contains(n.text, "clipboard") {
			copied = true
		}
	}
	if !copied || r.key == "" {
		t.Errorf("y did not copy the key, or hid it")
	}
	r.keyPress("x")
	if r.key != "" || a.View().MouseMode != tea.MouseModeCellMotion {
		t.Errorf("x did not hide the key and give the mouse back")
	}
}
