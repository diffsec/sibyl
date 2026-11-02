package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
)

// mouseClick presses and releases the left button on one cell.
func mouseClick(a *App, x, y int) {
	a.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func TestSelectedText(t *testing.T) {
	lines := []string{"Key for laptop:", "sib-0123abcd   ", "next line"}
	// A drag from the end of line 1 back to its start reads the same.
	s := selection{moved: true, x0: 11, y0: 1, x1: 0, y1: 1}
	if got := selectedText(lines, s); got != "sib-0123abcd" {
		t.Errorf("one line %q", got)
	}
	s = selection{moved: true, x0: 8, y0: 0, x1: 3, y1: 2}
	if got := selectedText(lines, s); got != "laptop:\nsib-0123abcd\nnext" {
		t.Errorf("three lines %q", got)
	}
	out := highlight([]string{"abc"}, selection{moved: true, x0: 1, y0: 0, x1: 1, y1: 0})
	if ansi.Strip(out[0]) != "abc" || out[0] == "abc" {
		t.Errorf("highlight %q", out[0])
	}
}

func TestDragCopiesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := New(c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	a.active = 5
	r := a.views[5].(*routerView)
	r.key, r.keyFor = "sib-0123", "laptop"
	v := a.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode %v", v.MouseMode)
	}
	row := -1
	for i, l := range strings.Split(ansi.Strip(v.Content), "\n") {
		if l == "sib-0123" {
			row = i
		}
	}
	if row < 0 {
		t.Fatalf("the key is not on a line of its own:\n%s", ansi.Strip(v.Content))
	}
	a.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: 7, Y: row, Button: tea.MouseLeft})
	a.View()
	_, cmd := a.Update(tea.MouseReleaseMsg{X: 7, Y: row, Button: tea.MouseLeft})
	var copied bool
	for _, m := range drain(cmd) {
		if n, ok := m.(noteMsg); ok && n.text == "copied 8 characters to the clipboard" {
			copied = true
		}
	}
	if !copied || selectedText(a.frame, a.sel) != "sib-0123" {
		t.Errorf("drag over the key: copied %v, text %q", copied, selectedText(a.frame, a.sel))
	}
	if !strings.Contains(a.View().Content, selSpan.Render("sib-0123")) {
		t.Error("the copied text is not highlighted")
	}
	a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if a.sel.moved {
		t.Error("a key did not clear the highlight")
	}

	// y copies the key, and esc hides it.
	var yCopied bool
	for _, m := range drain(r.keyPress("y")) {
		if n, ok := m.(noteMsg); ok && n.ok && strings.Contains(n.text, "clipboard") {
			yCopied = true
		}
	}
	if !yCopied || r.key == "" {
		t.Error("y did not copy the key, or hid it")
	}
	r.keyPress("esc")
	if r.key != "" {
		t.Error("esc did not hide the key")
	}

	// The recovery key of a new pool on Host works the same way.
	h := a.views[4].(*hostView)
	h.secret = "AAAA-BBBB"
	if drain(h.key("y")) == nil || h.secret == "" {
		t.Error("y did not copy the recovery key, or hid it")
	}
	h.key("esc")
	if h.secret != "" {
		t.Error("esc did not hide the recovery key")
	}
}
