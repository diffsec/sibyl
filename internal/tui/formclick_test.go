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
	formClick(f, lineOf(t, f, "Last field"), 4)
	if !strings.Contains(focusTitle(), "Last field") {
		t.Fatalf("focus on %q after a click on the last field", focusTitle())
	}
	formClick(f, lineOf(t, f, "Second field"), 4)
	if !strings.Contains(focusTitle(), "Second field") {
		t.Fatalf("focus on %q after a click on the second field", focusTitle())
	}
	// A click under the last field moves to it, and does not submit.
	formClick(f, 500, 4)
	if f.State != huh.StateNormal || !strings.Contains(focusTitle(), "Last field") {
		t.Fatalf("state %v, focus %q after a click under the form", f.State, focusTitle())
	}
	// A click on an option of the focused list moves its cursor.
	formClick(f, lineOf(t, f, "Third field"), 4)
	formClick(f, lineOf(t, f, "three"), 4)
	if pick != "three" {
		t.Errorf("pick %q after a click on three", pick)
	}
}

func TestFormClickConfirmAndArrows(t *testing.T) {
	pick, on, del := "a", false, false
	f := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Pick").Inline(true).Options(huh.NewOptions("a", "b", "c")...).Value(&pick),
		huh.NewConfirm().Title("Calibrate").Description("desc").Value(&on),
		huh.NewConfirm().Title("Delete?").Affirmative("Turn on").Negative("Leave off").Value(&del),
	)).WithShowHelp(false).WithWidth(80)
	f.Init()
	// col returns the line and the column of text in the form's view.
	col := func(text string) (int, int) {
		t.Helper()
		for i, l := range strings.Split(ansi.Strip(f.View()), "\n") {
			if j := strings.Index(l, text); j >= 0 {
				return i, ansi.StringWidth(l[:j])
			}
		}
		t.Fatalf("no %q in:\n%s", text, ansi.Strip(f.View()))
		return 0, 0
	}
	// The right arrow of the inline list moves it to b.
	line, x := col(" →")
	drain(formClick(f, line, x+1))
	if f.GetFocusedField().(*huh.Select[string]).GetValue() != "b" {
		t.Errorf("the right arrow did not move the list")
	}
	// A click on Yes of the unfocused Calibrate field moves to it and sets it.
	line, x = col("Yes")
	formClick(f, line, x+1)
	if c, ok := f.GetFocusedField().(*huh.Confirm); !ok || c.GetValue() != true {
		t.Errorf("a click on Yes: focus %T, value %v", f.GetFocusedField(), f.GetFocusedField().GetValue())
	}
	line, x = col("No")
	formClick(f, line, x)
	if f.GetFocusedField().GetValue() != false {
		t.Error("a click on No did not set no")
	}
	// The last field: a click on its button sets it and does not submit.
	line, x = col("Turn on")
	formClick(f, line, x+2)
	if f.State != huh.StateNormal || f.GetFocusedField().GetValue() != true {
		t.Errorf("a click on Turn on: state %v, value %v", f.State, f.GetFocusedField().GetValue())
	}
}
