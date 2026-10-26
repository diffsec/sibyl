package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/models"
)

type modelsLoaded struct {
	models []client.Model
	hw     hardware.Config
	err    error
}

type modelSaved struct {
	id  string
	res client.ApplyResult
	err error
}

type modelDeleted struct {
	id  string
	err error
}

type modelsView struct {
	c      *client.Client
	list   []client.Model
	hw     hardware.Config
	err    error
	cursor int

	form     *huh.Form
	editing  string // model ID being edited or deleted
	deleting bool
	confirm  bool
	fields   settingsForm
}

func newModelsView(c *client.Client) *modelsView { return &modelsView{c: c} }

func (v *modelsView) activate() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m, err := v.c.Models(ctx)
		if err != nil {
			return modelsLoaded{err: err}
		}
		hw, err := v.c.Hardware(ctx)
		return modelsLoaded{models: m, hw: hw, err: err}
	}
}

func (v *modelsView) capturing() bool { return v.form != nil }

func (v *modelsView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	return "↑/↓ select · enter edit · b benchmark HIP vs Vulkan · d delete"
}

func (v *modelsView) selected() (client.Model, bool) {
	if v.cursor < 0 || v.cursor >= len(v.list) {
		return client.Model{}, false
	}
	return v.list[v.cursor], true
}

func optionalNumber(title, desc string, whole bool, value *string) *huh.Input {
	return huh.NewInput().Title(title).Description(desc).Placeholder("model default").
		Value(value).Validate(numberCheck(whole))
}

// editForm builds the settings form for one model.
func (v *modelsView) editForm(m client.Model) tea.Cmd {
	v.editing = m.ID
	v.deleting = false
	v.fields = formFromSettings(m.Settings)
	f := &v.fields

	layouts := []huh.Option[string]{huh.NewOption("Hardware default ("+string(v.hw.Layout)+")", ""),
		huh.NewOption("Span all selected GPUs", "span")}
	for i := range v.hw.GPUs {
		layouts = append(layouts, huh.NewOption(fmt.Sprintf("Pin to selected GPU %d", i), "gpu:"+strconv.Itoa(i)))
	}
	var specOpts []huh.Option[string]
	for _, t := range models.SpecTypes {
		if t != "none" {
			specOpts = append(specOpts, huh.NewOption(t, t))
		}
	}
	drafts := []huh.Option[string]{huh.NewOption("None", "")}
	for _, o := range v.list {
		if o.ID != m.ID {
			drafts = append(drafts, huh.NewOption(o.ID, o.ID))
		}
	}
	kv := []huh.Option[string]{huh.NewOption("Default (f16)", "")}
	for _, t := range models.CacheTypes {
		kv = append(kv, huh.NewOption(t, t))
	}

	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title(m.ID).Description(fmt.Sprintf("%s · %d shard(s)", gib(m.Size), m.Shards)),
			huh.NewSelect[string]().Title("Layout").Inline(true).Options(layouts...).Value(&f.Layout),
			optionalNumber("Context size", "Tokens. Larger needs more VRAM for the KV cache.", true, &f.CtxSize),
			huh.NewSelect[string]().Title("KV cache K type").Inline(true).Options(kv...).Value(&f.KVTypeK),
			huh.NewSelect[string]().Title("KV cache V type").Inline(true).Options(kv...).Value(&f.KVTypeV),
			huh.NewConfirm().Title("Embedding model").Value(&f.Embedding),
			huh.NewConfirm().Title("Hide from the model list").Value(&f.Hidden),
		).Title("Model"),
		huh.NewGroup(
			optionalNumber("Temperature", "0 to 5", false, &f.Temperature),
			optionalNumber("Top K", "0 to 1000", true, &f.TopK),
			optionalNumber("Top P", "0 to 1", false, &f.TopP),
			optionalNumber("Min P", "0 to 1", false, &f.MinP),
			optionalNumber("Repeat penalty", "0 to 5", false, &f.RepeatPenalty),
			optionalNumber("Presence penalty", "-2 to 2", false, &f.PresencePenalty),
		).Title("Sampling defaults").Description("Clients can still override these per request."),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Speculative decoding types").
				Description("Tried in order. draft-* types except draft-mtp need a draft model.").
				Options(specOpts...).Value(&f.SpecTypes),
			huh.NewSelect[string]().Title("Draft model").Inline(true).Options(drafts...).Value(&f.DraftModel),
			optionalNumber("Draft tokens, max", "1 to 64", true, &f.DraftMax),
			optionalNumber("Draft tokens, min", "0 to 64", true, &f.DraftMin),
			optionalNumber("ngram-mod n min", "1 to 64", true, &f.NgramModMin),
			optionalNumber("ngram-mod n max", "1 to 64", true, &f.NgramModMax),
		).Title("Speculative decoding"),
		huh.NewGroup(
			huh.NewInput().Title("Extra llama-server arguments").
				Description("Space separated. Flags with a setting above are rejected.").Value(&f.Args),
		).Title("Advanced"),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *modelsView) deleteForm(m client.Model) tea.Cmd {
	v.editing = m.ID
	v.deleting = true
	v.confirm = false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete " + m.ID + "?").
			Description("This removes " + gib(m.Size) + " of model files from disk.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *modelsView) submit() tea.Cmd {
	id := v.editing
	if v.deleting {
		if !v.confirm {
			return note(true, "kept %s", id)
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := v.c.DeleteModel(ctx, id)
			return modelDeleted{id, err}
		}
	}
	s, err := v.fields.toSettings()
	if err != nil {
		return note(false, "%v", err)
	}
	return call(func(ctx context.Context) (client.ApplyResult, error) { return v.c.SetModel(ctx, id, s) },
		func(r client.ApplyResult, err error) tea.Msg { return modelSaved{id, r, err} })
}

func (v *modelsView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case modelsLoaded:
		v.err = msg.err
		if msg.err == nil {
			v.list, v.hw = msg.models, msg.hw
		}
		if v.cursor >= len(v.list) {
			v.cursor = max(0, len(v.list)-1)
		}
		return nil
	case modelSaved:
		if msg.err != nil {
			return note(false, "save %s: %v", msg.id, msg.err)
		}
		text := "saved " + msg.id
		for _, is := range msg.res.Issues {
			text += "; " + is.ID + ": " + is.Reason
		}
		return tea.Batch(note(len(msg.res.Issues) == 0, "%s", text), v.activate())
	case modelDeleted:
		if msg.err != nil {
			return note(false, "delete %s: %v", msg.id, msg.err)
		}
		return tea.Batch(note(true, "deleted %s", msg.id), v.activate())
	case tea.KeyPressMsg:
		if v.form == nil {
			switch msg.String() {
			case "up", "k":
				if v.cursor > 0 {
					v.cursor--
				}
			case "down", "j":
				if v.cursor < len(v.list)-1 {
					v.cursor++
				}
			case "enter", "e":
				if m, ok := v.selected(); ok {
					return v.editForm(m)
				}
			case "d":
				if m, ok := v.selected(); ok {
					return v.deleteForm(m)
				}
			case "b":
				if m, ok := v.selected(); ok {
					id := m.ID
					return func() tea.Msg {
						ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						if err := v.c.StartBenchmark(ctx, id); err != nil {
							return noteMsg{"benchmark " + id + ": " + err.Error(), false}
						}
						return noteMsg{"benchmark of " + id + " queued; it unloads running models and takes a few minutes", true}
					}
				}
			}
			return nil
		}
		if msg.String() == "esc" {
			v.form = nil
			return note(false, "changes discarded")
		}
	}
	if v.form == nil {
		return nil
	}
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	switch v.form.State {
	case huh.StateCompleted:
		v.form = nil
		return tea.Batch(cmd, v.submit())
	case huh.StateAborted:
		v.form = nil
	}
	return cmd
}

func summary(s models.Settings) string {
	var parts []string
	if s.Layout != "" {
		parts = append(parts, s.Layout)
	}
	if s.CtxSize > 0 {
		parts = append(parts, fmt.Sprintf("ctx %dk", s.CtxSize/1024))
	}
	if s.Embedding {
		parts = append(parts, "embedding")
	}
	if s.Sampling.Temperature != nil {
		parts = append(parts, "temp "+fmtFloat(s.Sampling.Temperature))
	}
	if len(s.Speculative.Types) > 0 {
		parts = append(parts, "spec "+strings.Join(s.Speculative.Types, ","))
	}
	if s.KVCache.TypeK != "" {
		parts = append(parts, "kv "+s.KVCache.TypeK)
	}
	return strings.Join(parts, " · ")
}

func (v *modelsView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.err != nil {
		b.WriteString(badSty.Render("API error: "+v.err.Error()) + "\n")
	}
	b.WriteString(titleSty.Render("Installed models") + "\n\n")
	if len(v.list) == 0 {
		b.WriteString(mutedSty.Render("  No models yet. Use the Download tab.") + "\n")
	}
	start, end := window(len(v.list), v.cursor)
	for i := start; i < end; i++ {
		m := v.list[i]
		line := fmt.Sprintf("%-40s %10s  %s", m.ID, gib(m.Size), mutedSty.Render(summary(m.Settings)))
		b.WriteString(cursorLine(i == v.cursor, line) + "\n")
		if bench := benchLine(m.Benchmark); bench != "" {
			b.WriteString("    " + mutedSty.Render(bench) + "\n")
		}
	}
	b.WriteString(more(start, end, len(v.list)))
	return b.String()
}

// benchLine summarises a model's latest benchmark.
func benchLine(b *client.Benchmark) string {
	if b == nil {
		return ""
	}
	if b.Status != "done" {
		return "benchmark " + b.Status
	}
	var parts []string
	for _, backend := range []string{"hip", "vulkan"} {
		r := b.Results[backend]
		if r.Error != "" {
			parts = append(parts, backend+" failed")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.1f tok/s gen, %.0f tok/s prompt", backend, r.GenTPS, r.PromptTPS))
	}
	line := "benchmark: " + strings.Join(parts, " · ")
	if b.Recommended != "" {
		line += " · faster: " + b.Recommended
	}
	return line
}
