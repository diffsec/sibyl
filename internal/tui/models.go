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
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

type modelsLoaded struct {
	models   []client.Model
	hw       hardware.Config
	gpus     []gpu.GPU
	advisors []string
	err      error
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
	rows   rowMap // the line of each list row, for clicks
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
	// action is an optimizer form: optimize, cancel-optimize or
	// apply-optimize. optProfile and optBackend are its answers.
	action     string
	optProfile string
	optBackend bool
	optAdvisor string
	// advisors are the router provider models that can advise the
	// optimizer.
	advisors []string
	fields   settingsForm
	// orig is fields when the form opened, and secs the sections of the
	// form, for the section bar.
	orig settingsForm
	secs []settingsSection
	bar  []zone
	// flow is the drafter finder, while it runs.
	flow *drafterFlow
	// running is what llama-swap runs. The list refreshes it every 5
	// seconds while the screen is open.
	running client.Running
	ticks   bool
}

type runningTick struct{}

func newModelsView(c *client.Client) *modelsView { return &modelsView{c: c} }

func (v *modelsView) activate() tea.Cmd {
	cmds := []tea.Cmd{v.fetchRunning(), v.fetchModels()}
	if !v.ticks {
		v.ticks = true
		cmds = append(cmds, tea.Tick(5*time.Second, func(time.Time) tea.Msg { return runningTick{} }))
	}
	return tea.Batch(cmds...)
}

func (v *modelsView) fetchModels() tea.Cmd {
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
		// Without the router, the optimizer offers no advisor.
		var advisors []string
		if r, err := v.c.Router(ctx); err == nil {
			advisors = client.AdvisorChoices(r)
		}
		return modelsLoaded{models: m, hw: hw, gpus: gpus, advisors: advisors}
	}
}

func (v *modelsView) capturing() bool { return v.form != nil }

func (v *modelsView) help() string {
	if v.form != nil {
		if v.saveable() {
			return formHelp(true) + " · pgup/pgdn section"
		}
		return formHelp(false)
	}
	return "↑/↓ select · enter edit · l load or unload · f find a drafter · b benchmark · o optimize · a apply · d delete"
}

// saveable is true for the settings form, which saveKey saves.
func (v *modelsView) saveable() bool {
	return v.form != nil && v.flow == nil && !v.deleting && v.action == ""
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
	return v.editFormWith(m, formFromSettings(m.Settings))
}

// editFormWith builds the settings form with the given values, such as
// the edits of a save that failed its checks.
func (v *modelsView) editFormWith(m client.Model, fields settingsForm) tea.Cmd {
	v.editing = m.ID
	v.deleting, v.action = false, ""
	v.fields = copySettings(fields)
	v.orig, v.secs = formFromSettings(m.Settings), visibleSections(m)
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
	list := v.list
	drafts := func() []huh.Option[string] { return draftChoices(list, m.ID, f.SpecTypes, f.DraftModel) }
	kv := []huh.Option[string]{huh.NewOption("Default (f16)", "")}
	for _, t := range models.CacheTypes {
		kv = append(kv, huh.NewOption(t, t))
	}

	if m.Engine == models.EngineEdge0 {
		return v.edge0Form(m, layouts)
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
			optionalNumber("Batch size", "", true, &f.Batch).Placeholder("llama.cpp default"),
			optionalNumber("Micro-batch size", "", true, &f.UBatch).Placeholder("llama.cpp default").
				Description("A larger micro-batch makes the prompt faster and uses more GPU memory."),
			huh.NewSelect[string]().Title("Split mode").Inline(true).Options(
				huh.NewOption("Layer", ""), huh.NewOption("Row", "row"),
			).Value(&f.SplitMode).Description("Only for a model that spans GPUs."),
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
		reasoningGroup(m, f),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Speculative decoding types").
				Description("Tried in order. draft-* types except draft-mtp need a draft model.").
				Options(specOpts...).Value(&f.SpecTypes).
				// See the GPU list in hardware.go: huh needs an explicit height.
				Height(len(specOpts)+3),
			huh.NewSelect[string]().Title("Draft model").Inline(true).
				Description("A drafter needs its type ticked above. Tick draft-simple to list the plain models.").
				OptionsFunc(drafts, &f.SpecTypes).Value(&f.DraftModel),
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

// edge0Form builds the settings form for an Edge0 model. Its engine is not
// llama.cpp, so the form holds only the settings that apply to it, the
// fields of edge0Sections.
func (v *modelsView) edge0Form(m client.Model, layouts []huh.Option[string]) tea.Cmd {
	f := &v.fields
	head := gib(m.Size) + " · Edge0 model folder"
	if m.Context > 0 {
		head += " · native context " + thousands(m.Context) + " tokens"
	}
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title(m.ID).Description(head),
			huh.NewSelect[string]().Title("Layout").Inline(true).Options(layouts...).Value(&f.Layout).
				Description("Span runs the model as a layer split across the GPUs. Pin runs it on one GPU, and other models can use the rest."),
			optionalNumber("Context size", "", true, &f.CtxSize).Placeholder(thousands(m.Edge0Context(0))).
				Description(edge0ContextNote(m)),
			huh.NewConfirm().Title("Hide from the model list").Value(&f.Hidden),
		).Title("Model"),
	).WithShowHelp(false)
	return v.form.Init()
}

// reasoningGroup is the reasoning field, in the variable and values that
// the model's chat template reads. It is hidden when the template has no
// reasoning control that Sibyl knows.
func reasoningGroup(m client.Model, f *settingsForm) *huh.Group {
	var c *models.ReasoningControl
	if m.Meta != nil {
		c = m.Meta.Reasoning
	}
	if c == nil {
		return huh.NewGroup(huh.NewNote()).WithHideFunc(func() bool { return true })
	}
	f.ReasoningVar = c.Var
	desc := "The chat template reads " + c.Var + ". A request can still set its own."
	var field huh.Field
	if choices := c.Choices(); len(choices) > 0 && (c.Switch || len(c.Values) > 0) {
		o := []huh.Option[string]{huh.NewOption("Template default", "")}
		for _, ch := range choices {
			o = append(o, huh.NewOption(ch.Value, ch.Var+"="+ch.Value))
		}
		field = huh.NewSelect[string]().Title("Reasoning").Inline(true).Options(o...).Value(&f.Reasoning).Description(desc)
	} else {
		field = huh.NewInput().Title("Reasoning").Placeholder("template default").Value(&f.Reasoning).
			Description(desc + " Sibyl found no list of its values, so type one.")
	}
	return huh.NewGroup(field).Title("Reasoning")
}

func (v *modelsView) deleteForm(m client.Model) tea.Cmd {
	v.editing = m.ID
	v.deleting, v.action = true, ""
	v.confirm = false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete " + m.ID + "?").
			Description("This removes " + gib(m.Size) + " of model files from disk.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *modelsView) submit() tea.Cmd {
	if v.flow != nil {
		return v.submitFlow()
	}
	if v.action != "" {
		return v.submitOptimize()
	}
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
	if cmd, ok := v.updateFlow(msg); ok {
		return cmd
	}
	switch msg := msg.(type) {
	case runningLoaded:
		if msg.err == nil {
			v.running = msg.r
		}
		return nil
	case runningTick:
		cmds := []tea.Cmd{v.fetchRunning(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return runningTick{} })}
		// A running optimizer changes its stage and results: show them.
		if v.optimizing() && v.form == nil {
			cmds = append(cmds, v.fetchModels())
		}
		return tea.Batch(cmds...)
	case modelsLoaded:
		v.err = msg.err
		if msg.err == nil {
			v.list, v.hw, v.gpus, v.advisors = client.OrderWithDrafters(msg.models), msg.hw, msg.gpus, msg.advisors
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
			case "f":
				if m, ok := v.selected(); ok {
					return v.findDrafters(m, false)
				}
			case "l":
				// l loads a model that is not loaded, and unloads one that
				// is loaded or loading.
				if m, ok := v.selected(); ok {
					return tea.Batch(v.loadAction(m, v.running.State(m.ID) == ""), v.fetchRunning())
				}
			case "o":
				if m, ok := v.selected(); ok {
					return v.optimizeForm(m)
				}
			case "a":
				if m, ok := v.selected(); ok {
					return v.applyForm(m)
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
			v.form, v.flow, v.action = nil, nil, ""
			return note(false, "changes discarded")
		}
		if k := msg.String(); (k == "pgup" || k == "pgdown") && v.saveable() {
			cur := currentSection(v.form.View(), v.secs)
			if k == "pgup" {
				return gotoSection(v.form, v.secs, cur-1)
			}
			return gotoSection(v.form, v.secs, cur+1)
		}
		if msg.String() == saveKey && v.saveable() {
			// A save that fails its checks keeps the form and its edits.
			if _, err := v.fields.Settings(); err != nil {
				return note(false, "%v", err)
			}
			v.form = nil
			return v.submit()
		}
	}
	if v.form == nil {
		return nil
	}
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	switch v.form.State {
	case huh.StateCompleted:
		settings := v.flow == nil && !v.deleting
		v.form = nil
		if settings {
			if _, err := v.fields.Settings(); err != nil {
				// huh ends a form on its last field, so the form opens
				// again with the edits.
				if i := slices.IndexFunc(v.list, func(o client.Model) bool { return o.ID == v.editing }); i >= 0 {
					return tea.Batch(cmd, v.editFormWith(v.list[i], v.fields), note(false, "%v", err))
				}
			}
		}
		return tea.Batch(cmd, v.submit())
	case huh.StateAborted:
		v.form, v.flow = nil, nil
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
	if r := s.Reasoning; r != nil {
		parts = append(parts, r.Var+" "+r.Value)
	}
	return strings.Join(parts, " · ")
}

func (v *modelsView) render(width int) string {
	if v.form != nil {
		if v.saveable() {
			view := v.form.View()
			bar, zones := sectionBar(v.secs, currentSection(view, v.secs), &v.orig, &v.fields)
			v.bar = zones
			return bar + "\n\n" + view
		}
		return v.form.View()
	}
	var b strings.Builder
	v.rows.reset()
	if v.err != nil {
		b.WriteString(badSty.Render("API error: "+v.err.Error()) + "\n")
	}
	loaded := 0
	for _, m := range v.list {
		if v.running.State(m.ID) == "ready" {
			loaded++
		}
	}
	fmt.Fprintf(&b, "%s  %s\n\n", titleSty.Render("Installed models"), mutedSty.Render(fmt.Sprintf("%d models · %d loaded", len(v.list), loaded)))
	if len(v.list) == 0 {
		b.WriteString(mutedSty.Render("  No models yet. Use the Download tab.") + "\n")
		return b.String()
	}
	top := strings.Count(b.String(), "\n")
	var list strings.Builder
	unusedShown := false
	for i, m := range v.list {
		if m.Drafter && drafterTarget(m, v.list) == "" && !unusedShown {
			list.WriteString(mutedSty.Render("Unused drafters") + "\n")
			unusedShown = true
		}
		name := m.ID
		if m.Drafter && drafterTarget(m, v.list) != "" {
			name = "↳ " + name
		}
		// The details show the whole name, so the list keeps the columns.
		name = ansi.Truncate(name, 34, "…")
		state := runningMark(v.running, m.ID)
		v.rows.lines[top+strings.Count(list.String(), "\n")] = i
		list.WriteString(cursorLine(i == v.cursor, fmt.Sprintf("%-34s %9s  %s", name, gib(m.Size), state)) + "\n")
	}
	m, _ := v.selected()
	b.WriteString(twoPane(list.String(), v.details(m), width))
	return b.String()
}

// drafterTarget is the model that uses drafter m, or "".
func drafterTarget(m client.Model, list []client.Model) string {
	for _, o := range list {
		if o.Settings.Speculative.DraftModel == m.ID {
			return o.ID
		}
	}
	return ""
}

// details describes the selected model.
func (v *modelsView) details(m client.Model) string {
	var b strings.Builder
	b.WriteString(titleSty.Render(m.ID) + "\n")
	files := fmt.Sprintf("%d file", m.Shards)
	if m.Shards != 1 {
		files += "s"
	}
	size := gib(m.Size) + " · " + files
	if m.Meta != nil {
		size += " · " + m.Meta.Architecture
	}
	fmt.Fprintf(&b, "Size      %s\n", size)
	if m.Drafter {
		if t := drafterTarget(m, v.list); t != "" {
			fmt.Fprintf(&b, "Drafter   for %s\n", t)
		} else {
			b.WriteString("Drafter   " + mutedSty.Render("no model uses it. d deletes it.") + "\n")
		}
		b.WriteString("\n" + mutedSty.Render("A drafter runs inside its model's llama-server, so it has no settings of its own.") + "\n")
		return b.String()
	}
	state := v.running.State(m.ID)
	switch state {
	case "":
		state = mutedSty.Render("not loaded · l loads it")
	case "ready":
		state = goodSty.Render("loaded") + mutedSty.Render(" · l unloads it")
	}
	fmt.Fprintf(&b, "State     %s\n", state)
	if m.Settings.Embedding {
		b.WriteString("Role      embedding model\n")
	}
	if f := m.Fit; f != nil {
		ctx := fmt.Sprintf("%s tokens", thousands(f.Ctx))
		if f.Auto {
			ctx += mutedSty.Render(" (auto)")
		}
		cache := "f16"
		if f.TypeK != "" {
			cache = f.TypeK
		}
		fmt.Fprintf(&b, "Context   %s · cache %s", ctx, cache)
		if f.Slots > 1 {
			fmt.Fprintf(&b, " · %d slots", f.Slots)
		}
		b.WriteString("\n")
		if !f.Fits {
			b.WriteString("          " + badSty.Render("part of it is in system RAM") + "\n")
		}
		if f.Note != "" {
			b.WriteString("          " + mutedSty.Render(f.Note) + "\n")
		}
	}
	layout := m.Settings.Layout
	if layout == "" {
		layout = "hardware default"
	}
	fmt.Fprintf(&b, "Layout    %s\n", layout)
	if sum := summary(m.Settings); sum != "" {
		fmt.Fprintf(&b, "Settings  %s\n", sum)
	}
	switch sp := m.Settings.Speculative; {
	case sp.DraftModel != "":
		fmt.Fprintf(&b, "Drafter   %s\n", sp.DraftModel)
	case m.Drafters != nil && *m.Drafters > 0:
		fmt.Fprintf(&b, "Drafter   %s\n", mutedSty.Render(fmt.Sprintf("none · %d on Hugging Face · f finds them", *m.Drafters)))
	}
	if bench := benchLine(m.Benchmark); bench != "" {
		b.WriteString("\n" + bench + "\n")
	}
	if opt := optimizeText(m.Optimize); opt != "" {
		b.WriteString("\n" + opt)
	}
	return b.String()
}

// benchGap is the smallest difference in generation speed that names a
// faster backend. Below it, the runs are within their own noise.
const benchGap = 0.05

// benchLine summarises a model's latest benchmark.
func benchLine(b *client.Benchmark) string {
	if b == nil {
		return ""
	}
	if b.Status != "done" {
		return "Benchmark " + b.Status
	}
	var lines []string
	var best, second float64
	bestName := ""
	for _, backend := range slices.Sorted(maps.Keys(b.Results)) {
		r := b.Results[backend]
		if r.Error != "" {
			lines = append(lines, fmt.Sprintf("  %-7s %s", backend, badSty.Render("failed")))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-7s %5.1f tok/s generation, %5.0f tok/s prompt", backend, r.GenTPS, r.PromptTPS))
		switch {
		case r.GenTPS > best:
			second, best, bestName = best, r.GenTPS, backend
		case r.GenTPS > second:
			second = r.GenTPS
		}
	}
	out := "Benchmark\n" + strings.Join(lines, "\n")
	switch {
	case bestName == "" || second == 0:
	case best >= second*(1+benchGap):
		out += fmt.Sprintf("\n  %s is %.0f%% faster", bestName, (best/second-1)*100)
	default:
		out += "\n  " + mutedSty.Render("the backends are within 5% of each other")
	}
	return out
}

func intAsFloat(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func (v *modelsView) clickLine(line int) tea.Cmd { return v.rows.click(line, &v.cursor, v.update) }

// clickForm takes a click on the section bar, or on the form under it.
func (v *modelsView) clickForm(line, x int) tea.Cmd {
	if !v.saveable() {
		return formClick(v.form, line, x)
	}
	if line == 0 {
		for _, z := range v.bar {
			if x >= z.x0 && x < z.x1 {
				return gotoSection(v.form, v.secs, z.tab)
			}
		}
	}
	if line < 2 {
		return nil
	}
	return formClick(v.form, line-2, x)
}

// draftChoices is the draft model list for target. It holds the drafters.
// A plain model can draft only with draft-simple, so the list holds them
// only when draft-simple is ticked, or when one is the current choice.
func draftChoices(list []client.Model, target string, types []string, current string) []huh.Option[string] {
	out := []huh.Option[string]{huh.NewOption("None", "")}
	simple := slices.Contains(types, "draft-simple")
	for _, o := range list {
		if o.ID == target || o.Settings.Embedding {
			continue
		}
		switch {
		case o.Drafter:
			label := o.ID
			if o.Meta != nil {
				if t := models.DrafterSpecType(o.Meta.Architecture); t != "" {
					label += " (" + t + ")"
				}
			}
			out = append(out, huh.NewOption(label, o.ID))
		case simple || o.ID == current:
			out = append(out, huh.NewOption(o.ID+" (draft-simple)", o.ID))
		}
	}
	return out
}
