package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/optimize"
)

// optimizeText is the optimizer part of a model's details.
func optimizeText(o *client.Optimize) string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Optimizer (" + string(o.Profile) + ")\n")
	switch o.Status {
	case "running":
		fmt.Fprintf(&b, "  %s · stage %s · %d settings measured\n", goodSty.Render("running"), o.Stage, measured(o.Results))
		b.WriteString("  " + mutedSty.Render("The models are unloaded until it ends. o cancels.") + "\n")
		return b.String()
	case "failed", "cancelled":
		b.WriteString("  " + badSty.Render(o.Status) + " " + o.Error + "\n")
		return b.String()
	}
	r := o.Recommendation
	if r == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "  now   %s\n", speeds(r.Baseline))
	fmt.Fprintf(&b, "  best  %s\n", speeds(r.Best))
	if len(r.Changes) == 0 {
		b.WriteString("  " + mutedSty.Render("The settings now are the fastest that were measured.") + "\n")
		return b.String()
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "  %-16s %s → %s\n", c.Setting, c.From, c.To)
	}
	switch {
	case !r.Verified:
		b.WriteString("  " + badSty.Render("The best settings did not load at the model's context.") + "\n")
	case o.Applied != "":
		b.WriteString("  " + mutedSty.Render("applied "+o.Applied) + "\n")
	default:
		b.WriteString("  " + mutedSty.Render("a applies these settings") + "\n")
	}
	return b.String()
}

func measured(rs []optimize.Result) int {
	n := 0
	for _, r := range rs {
		if r.Error == "" {
			n++
		}
	}
	return n
}

// speeds formats a result's numbers.
func speeds(r optimize.Result) string {
	if r.TotalTPS > 0 {
		return fmt.Sprintf("%5.1f tok/s total over %d requests, %5.1f tok/s each", r.TotalTPS, r.Parallel, r.RequestTPS)
	}
	return fmt.Sprintf("%5.1f tok/s generation, %5.0f tok/s prompt", r.GenTPS, r.PromptTPS)
}

// optimizeForm starts a run, or cancels the one that runs.
func (v *modelsView) optimizeForm(m client.Model) tea.Cmd {
	v.editing, v.confirm = m.ID, false
	if m.Optimize != nil && m.Optimize.Status == "running" {
		v.action = "cancel-optimize"
		v.form = huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Cancel the optimizer for " + m.ID + "?").
				Description("The tool that runs now finishes first. The measurements so far stay.").
				Affirmative("Cancel the run").Negative("Keep it running").Value(&v.confirm),
		)).WithShowHelp(false)
		return v.form.Init()
	}
	v.action, v.optProfile = "optimize", string(optimize.Single)
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Optimize "+m.ID+" for").Options(
			huh.NewOption("One user: generation speed, then prompt speed", string(optimize.Single)),
			huh.NewOption("Throughput: total speed over parallel requests", string(optimize.Throughput)),
		).Value(&v.optProfile),
		huh.NewConfirm().Title("Start the optimizer?").
			Description("It measures settings with llama-bench and checks the best one with llama-server.\n"+
				"The models are unloaded for 5 to 15 minutes. Nothing changes until you apply the result.").
			Affirmative("Start").Negative("Back").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// applyForm applies a finished run's recommendation.
func (v *modelsView) applyForm(m client.Model) tea.Cmd {
	o := m.Optimize
	if o == nil || o.Status != "done" || o.Recommendation == nil {
		return note(false, "no finished optimizer run for %s; o starts one", m.ID)
	}
	r := o.Recommendation
	if len(r.Changes) == 0 {
		return note(true, "%s already runs with the fastest measured settings", m.ID)
	}
	if !r.Verified {
		return note(false, "the best settings for %s did not load at its context, so they cannot be applied", m.ID)
	}
	v.editing, v.action, v.confirm, v.optBackend = m.ID, "apply-optimize", false, false
	fields := []huh.Field{
		huh.NewConfirm().Title("Apply the optimizer's settings to " + m.ID + "?").
			Description(fmt.Sprintf("Measured %s, from %s.", speeds(r.Best), strings.TrimSpace(speeds(r.Baseline)))).
			Affirmative("Apply").Negative("Back").Value(&v.confirm),
	}
	if r.Backend != "" {
		fields = append(fields, huh.NewConfirm().Title("Switch the backend to "+r.Backend+" too?").
			Description("The backend applies to every model, and llama-swap restarts.").
			Affirmative("Switch").Negative("Keep "+r.Baseline.Backend).Value(&v.optBackend))
	}
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

// submitOptimize runs the answer of an optimizer form.
func (v *modelsView) submitOptimize() tea.Cmd {
	id, action := v.editing, v.action
	v.action = ""
	if !v.confirm {
		return note(true, "nothing changed")
	}
	switch action {
	case "cancel-optimize":
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := v.c.CancelOptimize(ctx, id); err != nil {
				return noteMsg{"cancel the optimizer: " + err.Error(), false}
			}
			return noteMsg{"the optimizer for " + id + " stops after its current tool", true}
		}
	case "optimize":
		profile := optimize.Profile(v.optProfile)
		return tea.Batch(func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := v.c.StartOptimize(ctx, id, profile); err != nil {
				return noteMsg{"optimize " + id + ": " + err.Error(), false}
			}
			return noteMsg{"the optimizer for " + id + " started; it takes 5 to 15 minutes", true}
		}, v.fetchModels())
	}
	backend := v.optBackend
	return call(func(ctx context.Context) (client.ApplyResult, error) { return v.c.ApplyOptimize(ctx, id, backend) },
		func(r client.ApplyResult, err error) tea.Msg { return modelSaved{id, r, err} })
}

// optimizing tells whether a run goes for any model.
func (v *modelsView) optimizing() bool {
	for _, m := range v.list {
		if m.Optimize != nil && m.Optimize.Status == "running" {
			return true
		}
	}
	return false
}
