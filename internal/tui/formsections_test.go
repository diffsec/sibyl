package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

func TestSettingsSections(t *testing.T) {
	v := newModelsView(nil)
	v.list = []client.Model{{Model: models.Model{ID: "qwen"}}}
	v.editForm(v.list[0])
	bar := func() string { return strings.SplitN(ansi.Strip(v.render(120)), "\n", 2)[0] }
	cur := func() string { return v.secs[currentSection(v.form.View(), v.secs)].title }
	if got := bar(); got != "Model │ Sampling defaults │ Speculative decoding │ Advanced" {
		t.Fatalf("bar %q", got)
	}
	if cur() != "Model" {
		t.Fatalf("first section %q", cur())
	}
	pgdn := tea.KeyPressMsg{Code: tea.KeyPgDown}
	v.update(pgdn)
	if cur() != "Sampling defaults" {
		t.Errorf("pgdn went to %q", cur())
	}
	v.fields.Temperature = "0.6"
	if !strings.Contains(bar(), "Sampling defaults •") || strings.Contains(bar(), "Model •") {
		t.Errorf("change marks: %q", bar())
	}
	v.update(pgdn)
	v.update(pgdn)
	v.update(pgdn) // past the last section: the form stays open
	if cur() != "Advanced" || v.form == nil || v.form.State != 0 {
		t.Errorf("pgdn past the end: %q, form open %v", cur(), v.form != nil)
	}
	// A click on "Model" in the bar goes back to the first section.
	v.render(120)
	v.clickForm(0, 1)
	if cur() != "Model" {
		t.Errorf("a click on Model went to %q", cur())
	}
	v.update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if cur() != "Model" {
		t.Errorf("pgup on the first section went to %q", cur())
	}
}

func TestEdge0Form(t *testing.T) {
	v := newModelsView(nil)
	v.list = []client.Model{{Model: models.Model{ID: "edge0-120b-q3k", Engine: models.EngineEdge0, Context: 262144}}}
	drainForm(v.form, v.editForm(v.list[0]))
	view := ansi.Strip(v.form.View())
	for _, want := range []string{"Layout", "Context size", "131,072", "up to 262,144", "Hide from the model list"} {
		if !strings.Contains(view, want) {
			t.Errorf("the form has no %q:\n%s", want, view)
		}
	}
	// llama-server settings do not apply to the Edge0 engine.
	for _, unwanted := range []string{"Tensor split", "Parallel requests", "KV cache", "no native context", "Sampling defaults"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("the form shows %q", unwanted)
		}
	}
	v.update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if cur := currentSection(v.form.View(), v.secs); cur != 0 || v.form.State != 0 {
		t.Errorf("pgdn left the only section: section %d, state %v", cur, v.form.State)
	}
}
