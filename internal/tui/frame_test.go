package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

func TestParseHelpAndButtons(t *testing.T) {
	segs := parseHelp("↑/↓ select · enter edit · l load · / search · ctrl+s save · K new key")
	if len(segs) != 6 || !segs[0].hint || segs[1].hint || segs[3].key != "/" || segs[3].hint {
		t.Fatalf("segments %+v", segs)
	}
	lines, zones := buttonBar(segs, 30, 10)
	if len(lines) < 2 {
		t.Fatalf("30 columns did not wrap: %q", lines)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 30 {
			t.Errorf("line %q is %d wide", ansi.Strip(l), w)
		}
	}
	if len(zones) != 5 || zones[0].key != "enter" || zones[0].y != 10 {
		t.Fatalf("zones %+v", zones)
	}
	// Each zone covers its button's text.
	for _, z := range zones {
		text := ansi.Strip(lines[z.y-10])
		if got := ansi.Cut(text, z.x0, z.x1); !strings.HasPrefix(got, "["+z.key+"]") {
			t.Errorf("zone %+v covers %q", z, got)
		}
	}
}

func TestKeyMsg(t *testing.T) {
	for _, k := range []string{"enter", "esc", "ctrl+s", "shift+tab", "l", "K", "/"} {
		m, ok := keyMsg(k)
		if !ok || m.String() != k {
			t.Errorf("%q gives %q", k, m.String())
		}
	}
	if _, ok := keyMsg("↑/↓"); ok {
		t.Error("a hint gave a key")
	}
}

func TestScroller(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "row"
	}
	lines[25] = "› selected"
	s := &scroller{focus: -1}
	got := s.window(lines, 10)
	if len(got) != 10 || !strings.Contains(strings.Join(got, "\n"), "› selected") {
		t.Errorf("the cursor line is not in sight: offset %d", s.offset)
	}
	if !strings.Contains(ansi.Strip(got[9]), "more") && !strings.Contains(ansi.Strip(got[0]), "more") {
		t.Errorf("no more hint: %q", got)
	}
	// A cursor on the last row of the window is not under the hint.
	edge := make([]string, 20)
	for i := range edge {
		edge[i] = "row"
	}
	edge[9] = "› last"
	e := &scroller{focus: -1}
	if got := e.window(edge, 10); !strings.Contains(strings.Join(got, "\n"), "› last") {
		t.Errorf("the cursor on row 9 is hidden by the hint: offset %d", e.offset)
	}
	// The wheel moves the window, and the cursor does not pull it back
	// until it moves.
	s.scroll(-20)
	if s.window(lines, 10); s.offset != 0 {
		t.Errorf("offset %d after the wheel", s.offset)
	}
}

// TestFrameFitsSmallTerminal checks that at 80x24 the frame is 24 lines,
// and the buttons stay on the screen under a long view.
func TestFrameFitsSmallTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := New(c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	a.active = 2
	m := a.views[2].(*modelsView)
	for i := range 40 {
		m.list = append(m.list, client.Model{Model: models.Model{ID: "model-" + strings.Repeat("x", i%5) + string(rune('a'+i%26))}})
	}
	lines := strings.Split(a.View().Content, "\n")
	if len(lines) != 24 {
		t.Fatalf("%d lines, want 24", len(lines))
	}
	last := ansi.Strip(lines[len(lines)-1])
	if !strings.Contains(strings.Join(lines[len(lines)-3:], "\n"), "[q] quit") {
		t.Errorf("the buttons are off the screen; last line %q", last)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 80 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}
	// A click on the second list row selects it.
	row := -1
	for i, l := range lines {
		if strings.Contains(ansi.Strip(l), m.list[1].ID) {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatal("no second row on the screen")
	}
	mouseClick(a, 4, row)
	if m.cursor != 1 {
		t.Errorf("cursor %d after a click on row 1", m.cursor)
	}
	// A click on a tab switches to it.
	for _, z := range a.zones {
		if z.tab == 4 {
			mouseClick(a, z.x0, z.y)
		}
	}
	if a.active != 4 {
		t.Errorf("active tab %d after a click on Host", a.active)
	}
}
