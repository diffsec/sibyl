package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

// lineOf returns the line of the form's view that holds text.
func lineOf(t *testing.T, f *huh.Form, text string) int {
	t.Helper()
	for i, l := range strings.Split(ansi.Strip(f.View()), "\n") {
		if strings.Contains(l, text) {
			return i
		}
	}
	t.Fatalf("no line with %q in:\n%s", text, ansi.Strip(f.View()))
	return -1
}

func drainForm(f *huh.Form, cmd tea.Cmd) {
	for _, m := range drain(cmd) {
		f.Update(m)
	}
}

func TestFormClick(t *testing.T) {
	var a, b, c, pick string
	pick = "one"
	f := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("First field").Value(&a),
		huh.NewInput().Title("Second field").Value(&b),
		huh.NewSelect[string]().Title("Third field").Options(
			huh.NewOption("one", "one"), huh.NewOption("two", "two"), huh.NewOption("three", "three"),
		).Value(&pick),
		huh.NewInput().Title("Last field").Value(&c),
	)).WithShowHelp(false)
	drainForm(f, f.Init())

	focusTitle := func() string {
		first, _ := focusedBlock(f.View())
		return strings.Split(ansi.Strip(f.View()), "\n")[first]
	}
	formClick(f, lineOf(t, f, "Last field"))
	if !strings.Contains(focusTitle(), "Last field") {
		t.Fatalf("focus on %q after a click on the last field", focusTitle())
	}
	formClick(f, lineOf(t, f, "Second field"))
	if !strings.Contains(focusTitle(), "Second field") {
		t.Fatalf("focus on %q after a click on the second field", focusTitle())
	}
	// A click under the last field moves to it, and does not submit.
	formClick(f, 500)
	if f.State != huh.StateNormal || !strings.Contains(focusTitle(), "Last field") {
		t.Fatalf("state %v, focus %q after a click under the form", f.State, focusTitle())
	}
	// A click on an option of the focused list moves its cursor.
	formClick(f, lineOf(t, f, "Third field"))
	formClick(f, lineOf(t, f, "three"))
	if pick != "three" {
		t.Errorf("pick %q after a click on three", pick)
	}
}
