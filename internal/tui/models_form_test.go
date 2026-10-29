package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

// The settings form shows the native context and the memory estimate
// when it opens, and the sampling fields show the model file's defaults.
func TestEditFormShowsContextEstimate(t *testing.T) {
	temp, topK := 1.0, 20
	m := client.Model{Model: models.Model{ID: "big", Size: 29 << 30, Shards: 1}, Meta: &models.Meta{
		Architecture: "qwen35", ContextLength: 262144, KElems: 16384, VElems: 16384, MaxLayerKV: 2048,
		Sampling: models.Sampling{Temperature: &temp, TopK: &topK},
	}}
	v := newModelsView(nil)
	v.list = []client.Model{m}
	v.hw = hardware.Config{Backend: hardware.BackendHIP, GPUs: []int{0, 1}, Layout: hardware.LayoutSpan}
	v.gpus = []gpu.GPU{{Index: 0, VRAMTotal: 24 << 30}, {Index: 1, VRAMTotal: 24 << 30}}

	run(v, v.editForm(m), 0)
	v.update(tea.WindowSizeMsg{Width: 140, Height: 60})
	view := v.form.View()
	for _, want := range []string{"native context 262,144 tokens", "Empty: Sibyl uses", "When VRAM is short"} {
		if !strings.Contains(view, want) {
			t.Errorf("form view lacks %q:\n%s", want, view)
		}
	}

	// Typing a context size recomputes the description.
	send := func(msg tea.Msg) { run(v, v.update(msg), 0) }
	send(tea.KeyPressMsg{Code: tea.KeyTab}) // to the fit mode
	send(tea.KeyPressMsg{Code: tea.KeyTab}) // to the parallel requests
	send(tea.KeyPressMsg{Code: tea.KeyTab}) // to the context size
	for _, r := range "32768" {
		send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	view = v.form.View()
	if !strings.Contains(view, "At 32,768 tokens") || strings.Contains(view, "more than the GPUs hold") {
		t.Errorf("after typing 32768:\n%s", view)
	}
}

// run executes a command and feeds its messages back to the view, as the
// Bubble Tea runtime does, including the commands inside a batch. It skips
// commands that would block, such as the cursor blink.
func run(v *modelsView, cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 8 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(50 * time.Millisecond):
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			run(v, c, depth+1)
		}
		return
	}
	if msg != nil {
		run(v, v.update(msg), depth+1)
	}
}
