package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/hostd"
)

// windowStep is the Status screen's form step for the update window.
const windowStep = 3

// windowText describes the nightly update window.
func windowText(w *hostd.UpdateWindow) string {
	if w == nil {
		return ""
	}
	if !w.Enabled {
		return "nightly updates off · press w to set the window"
	}
	return fmt.Sprintf("nightly updates from %s to %s, host time, when the models are idle · press w to change", w.Start, w.End)
}

// windowForm sets the nightly update window: every 30 minutes from the
// start to the end, host time.
func (v *statusView) windowForm() tea.Cmd {
	w := hostd.UpdateWindow{Enabled: true, Start: hostd.DefaultWindowStart, End: hostd.DefaultWindowEnd}
	if u := v.data.host.Update; u != nil && u.Window != nil {
		w = *u.Window
	}
	v.step, v.mode, v.win = windowStep, "window", w
	times := make([]huh.Option[string], 0, 48)
	for i := range 48 {
		t := fmt.Sprintf("%02d:%02d", i/2, i%2*30)
		times = append(times, huh.NewOption(t, t))
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Nightly updates").
			Description("Sibyl stages a new image and reboots into it in the window, when no request ran for 30 minutes.").
			Affirmative("On").Negative("Off").Value(&v.win.Enabled),
		huh.NewSelect[string]().Title("First run").Description("Host time.").Options(times...).Height(8).Value(&v.win.Start),
		huh.NewSelect[string]().Title("Last run").Description("It runs every 30 minutes from the first run to this one.").
			Options(times...).Height(8).Value(&v.win.End),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *statusView) saveWindow() tea.Cmd {
	v.form = nil
	w := v.win
	return v.hostCall("update window", func(ctx context.Context) (hostd.Result, error) { return v.c.SetUpdateWindow(ctx, w) })
}
