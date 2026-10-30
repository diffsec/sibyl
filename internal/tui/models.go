package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

type modelsLoaded struct {
	models []client.Model
	hw     hardware.Config
	gpus   []gpu.GPU
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
	gpus   []gpu.GPU // detected GPUs, for the memory estimate
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
		if err != nil {
			return modelsLoaded{err: err}
		}
		// Without the GPU list the estimate leaves out the VRAM total.
		gpus, _ := v.c.GPUs(ctx)
		return modelsLoaded{models: m, hw: hw, gpus: gpus}
	}
}

func (v *modelsView) capturing() bool { return v.form != nil }

func (v *modelsView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	return "↑/↓ select · enter edit · b benchmark the backends · d delete"
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

	head := fmt.Sprintf("%s · %d shard(s)", gib(m.Size), m.Shards)
	var fileSampling models.Sampling
	if m.Meta != nil {
		head += fmt.Sprintf(" · %s · native context %s tokens", m.Meta.Architecture, thousands(m.Meta.ContextLength))
		fileSampling = m.Meta.Sampling
	}
	var fileTopK *float64
	if fileSampling.TopK != nil {
		k := float64(*fileSampling.TopK)
		fileTopK = &k
	}
	hw, gpus := v.hw, v.gpus
	presetOpts := []huh.Option[string]{huh.NewOption("None", "")}
	for _, p := range models.Presets {
		presetOpts = append(presetOpts, huh.NewOption(p, p))
	}
	// sampleField shows, in grey, the value that applies when the field is
	// empty: the preset's, else the model file's, else llama.cpp's.
	sampleField := func(title, desc string, whole bool, value *string, pick func(models.Sampling) *float64,
		file *float64, llamaDefault string) *huh.Input {
		return optionalNumber(title, desc, whole, value).PlaceholderFunc(func() string {
			if f.Preset == "" {
				return samplingHint(file, llamaDefault)
			}
			ps, err := models.PresetSampling(f.Preset, m.Meta)
			if err != nil {
				return samplingHint(file, llamaDefault)
			}
			return presetHint(f.Preset, pick(ps))
		}, &f.Preset)
	}

	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title(m.ID).Description(head),
			huh.NewSelect[string]().Title("Layout").Inline(true).Options(layouts...).Value(&f.Layout),
			huh.NewSelect[string]().Title("When VRAM is short").Inline(true).Options(
				huh.NewOption("Quality: f16 cache, shorter context", ""),
				huh.NewOption("Context: quantize the cache to keep the context", string(models.FitContext)),
			).Value(&f.Fit),
			optionalNumber("Parallel requests", "", true, &f.Parallel).Placeholder("llama.cpp default").
				Description("Slots that answer at once. They share the context, so each is sure of the context divided by the slots."),
			optionalNumber("Context size", "", true, &f.CtxSize).Placeholder("auto").
				DescriptionFunc(func() string { return contextNote(m, f, hw, gpus) }, f),
			huh.NewInput().Title("Tensor split").Placeholder(splitHint(m)).Value(&f.TensorSplit).
				Description("The share of the layers on each GPU, such as 3,2. Empty splits by the memory that each GPU has left."),
			huh.NewSelect[string]().Title("KV cache K type").Inline(true).Options(kv...).Value(&f.KVTypeK),
			huh.NewSelect[string]().Title("KV cache V type").Inline(true).Options(kv...).Value(&f.KVTypeV),
			huh.NewConfirm().Title("Embedding model").Value(&f.Embedding),
			huh.NewConfirm().Title("Hide from the model list").Value(&f.Hidden),
		).Title("Model"),
		huh.NewGroup(
			huh.NewInput().Title("Experts in system RAM").Placeholder("none").Value(&f.MoELayers).
				Validate(func(v string) error { _, err := parseMoELayers(v); return err }).
				DescriptionFunc(func() string { return moeNote(m, f) }, f),
			optionalNumber("Expert GPU cache, MiB", "", true, &f.MoECacheMiB).Placeholder("off").
				Description("GPU memory that keeps the most used experts from system RAM. It needs experts in system RAM.\n"+
					"Context size counts it, so a larger cache gives a shorter automatic context."),
		).Title("Mixture of Experts").WithHideFunc(func() bool { return m.Meta == nil || m.Meta.ExpertCount == 0 }),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Preset").Inline(true).Options(presetOpts...).Value(&f.Preset).
				Description(presetNote(m.Meta)),
			sampleField("Temperature", "0 to 5", false, &f.Temperature, func(s models.Sampling) *float64 { return s.Temperature }, fileSampling.Temperature, "0.8"),
			sampleField("Top K", "0 to 1000", true, &f.TopK, func(s models.Sampling) *float64 { return intAsFloat(s.TopK) }, fileTopK, "40"),
			sampleField("Top P", "0 to 1", false, &f.TopP, func(s models.Sampling) *float64 { return s.TopP }, fileSampling.TopP, "0.95"),
			sampleField("Min P", "0 to 1", false, &f.MinP, func(s models.Sampling) *float64 { return s.MinP }, fileSampling.MinP, "0.05"),
			optionalNumber("Repeat penalty", "0 to 5", false, &f.RepeatPenalty),
			optionalNumber("Presence penalty", "-2 to 2", false, &f.PresencePenalty),
		).Title("Sampling defaults").Description("Empty uses the value in grey. Clients can still override these per request."),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Speculative decoding types").
				Description("Tried in order. draft-* types except draft-mtp need a draft model.").
				Options(specOpts...).Value(&f.SpecTypes).
				// See the GPU list in hardware.go: huh needs an explicit height.
				Height(len(specOpts)+3),
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
	s, err := v.fields.Settings()
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
			v.list, v.hw, v.gpus = msg.models, msg.hw, msg.gpus
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
	backends := slices.Sorted(maps.Keys(b.Results))
	for _, backend := range backends {
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

func intAsFloat(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}
