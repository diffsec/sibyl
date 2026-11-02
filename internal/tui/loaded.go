package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
)

// loadedText lists the models that llama-swap runs, the loads that wait,
// and the last failed loads.
func loadedText(r *client.Running) string {
	if r == nil {
		return "  " + mutedSty.Render("llama-swap does not answer") + "\n"
	}
	if len(r.Models) == 0 && len(r.Loading) == 0 && len(r.Errors) == 0 {
		return "  " + mutedSty.Render("none: a request or l on the Models screen loads a model") + "\n"
	}
	var b strings.Builder
	for _, m := range r.Models {
		state := m.State
		if state == "ready" {
			state = goodSty.Render(state)
		}
		b.WriteString("  " + m.ID + "  " + state + "\n")
	}
	for _, id := range r.Loading {
		if !slices.ContainsFunc(r.Models, func(m client.RunningModel) bool { return m.ID == id }) {
			b.WriteString("  " + id + "  " + mutedSty.Render("loading") + "\n")
		}
	}
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for id := range r.Errors {
			if !yield(id) {
				return
			}
		}
	}) {
		b.WriteString("  " + badSty.Render(id+": the last load failed: "+r.Errors[id]) + "\n")
	}
	return b.String()
}

type runningLoaded struct {
	r   client.Running
	err error
}

func (v *modelsView) fetchRunning() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, err := v.c.Running(ctx)
		return runningLoaded{r, err}
	}
}

// loadAction loads or unloads the selected model.
func (v *modelsView) loadAction(m client.Model, load bool) tea.Cmd {
	if m.Drafter {
		return note(false, "%s is a drafter; it loads with the model that uses it", m.ID)
	}
	id := m.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if !load {
			if err := v.c.UnloadModel(ctx, id); err != nil {
				return noteMsg{"unload " + id + ": " + err.Error(), false}
			}
			return noteMsg{"unloaded " + id, true}
		}
		if err := v.c.LoadModel(ctx, id); err != nil {
			return noteMsg{"load " + id + ": " + err.Error(), false}
		}
		return noteMsg{"loading " + id + "; llama-swap stops the models that cannot run with it", true}
	}
}

// runningMark is a model's load state in the model list.
func runningMark(r client.Running, id string) string {
	switch s := r.State(id); s {
	case "":
		return ""
	case "ready":
		return goodSty.Render("loaded")
	default:
		return mutedSty.Render(s)
	}
}
