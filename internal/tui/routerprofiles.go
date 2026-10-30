package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/router"
)

// The values of the profile, rule, calibration and classifier forms.
type profileForm struct {
	profile, action, name string
	model, fallback, wait string
	// pick is "<profile>\x00<rule index>" or "<profile>\x00add".
	pick string
	// The rule form.
	ruleName, route, sensitive, needs, images, tools, promptOver string
	tasks, diffs, clients                                        []string
	// The calibration form.
	rate, daily, judge, compare, keepDays, maxSamples string
	seed, autoApply                                   bool
	// The classifier form.
	words, clsModel, k, timeout string
	useStarter                  bool
}

// modelsForForm carries the installed models for a form that picks one.
type modelsForForm struct {
	models []client.Model
	err    error
	next   string
}

const (
	newProfile = "\x00new"
	addRule    = "add"
	anyValue   = "any"
)

// profileNames returns the profile names in order.
func (v *routerView) profileNames() []string {
	names := make([]string, 0, len(v.r.Profiles.Profiles))
	for n := range v.r.Profiles.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// profilesCopy returns a deep copy of the profiles file to change.
func (v *routerView) profilesCopy() (router.ProfilesFile, error) {
	var f router.ProfilesFile
	b, err := json.Marshal(v.r.Profiles)
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]router.Profile{}
	}
	return f, err
}

func (v *routerView) saveProfiles(what string, f router.ProfilesFile) tea.Cmd {
	return v.run(what, func(ctx context.Context) (string, error) { return "", v.c.SetRouterProfiles(ctx, f) })
}

// loadModels fetches the installed models, then opens the form next.
func (v *routerView) loadModels(next string) tea.Cmd {
	return call(v.c.Models, func(m []client.Model, err error) tea.Msg { return modelsForForm{m, err, next} })
}

func (v *routerView) modelsLoaded(msg modelsForForm) tea.Cmd {
	if msg.err != nil {
		return note(false, "models: %v", msg.err)
	}
	v.models = msg.models
	switch msg.next {
	case "profilepick":
		return v.profilePickForm()
	case "classifier":
		return v.classifierForm()
	}
	return nil
}

// modelOptions lists the installed chat models, or the embedding models.
func (v *routerView) modelOptions(embedding bool, current string) []huh.Option[string] {
	var opts []huh.Option[string]
	seen := map[string]bool{}
	for _, m := range v.models {
		if m.Settings.Embedding != embedding {
			continue
		}
		opts = append(opts, huh.NewOption(m.ID, m.ID))
		seen[m.ID] = true
	}
	if current != "" && !seen[current] {
		opts = append(opts, huh.NewOption(current+" (not installed)", current))
	}
	return opts
}

// routeOptions lists "local", the listed provider models and the routes
// that the profiles already use.
func (v *routerView) routeOptions(current string) []huh.Option[string] {
	opts := []huh.Option[string]{huh.NewOption("local: the profile's local model", "local")}
	seen := map[string]bool{"local": true}
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			opts = append(opts, huh.NewOption(r, r))
		}
	}
	for _, p := range v.r.Providers {
		for _, m := range p.Models {
			add(p.Name + "/" + m)
		}
	}
	for _, n := range v.profileNames() {
		p := v.r.Profiles.Profiles[n]
		for _, r := range p.Rules {
			add(r.Route)
		}
		for _, r := range p.Fallback {
			add(r)
		}
	}
	add(current)
	return opts
}

func validInt(lo, hi int) func(string) error {
	return func(s string) error {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		if n, err := strconv.Atoi(s); err != nil || n < lo || n > hi {
			return fmt.Errorf("a whole number from %d to %d, or empty for the default", lo, hi)
		}
		return nil
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// The profile forms.

func (v *routerView) profilePickForm() tea.Cmd {
	opts := []huh.Option[string]{huh.NewOption("New profile", newProfile)}
	for _, n := range v.profileNames() {
		model := n
		if n != "auto" {
			model = "auto:" + n
		}
		opts = append(opts, huh.NewOption(model+" · local "+v.r.Profiles.Profiles[n].LocalModel, n))
	}
	v.mode, v.pf.profile, v.pf.action = "profilepick", opts[0].Value, "settings"
	if len(opts) > 1 {
		v.pf.profile = opts[1].Value
	}
	v.form = huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title("Profile").
			Description("A client asks for a profile with the model auto or auto:<name>.").
			Options(opts...).Value(&v.pf.profile)),
		huh.NewGroup(huh.NewSelect[string]().Title("Change").Options(
			huh.NewOption("Local model, fallback and wait", "settings"),
			huh.NewOption("Delete the profile", "delete"),
		).Value(&v.pf.action)).WithHideFunc(func() bool { return v.pf.profile == newProfile }),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) profileForm() tea.Cmd {
	p := v.r.Profiles.Profiles[v.pf.profile]
	v.mode = "profile"
	v.pf.name, v.pf.model, v.pf.wait = "", p.LocalModel, itoa(p.LocalWaitSeconds)
	v.pf.fallback = strings.Join(p.Fallback, " ")
	models := v.modelOptions(false, p.LocalModel)
	if len(models) == 0 {
		return note(false, "no chat model is installed")
	}
	if v.pf.model == "" {
		v.pf.model = models[0].Value
	}
	var fields []huh.Field
	if v.pf.profile == newProfile {
		fields = append(fields, huh.NewInput().Title("Profile name").
			Description("auto is the profile of the model auto. Another name is the model auto:<name>.").
			Value(&v.pf.name).Validate(func(s string) error {
			if _, ok := v.r.Profiles.Profiles[strings.TrimSpace(s)]; ok {
				return errors.New("that profile exists")
			}
			return validName(s)
		}))
	} else {
		fields = append(fields, huh.NewNote().Title("Profile "+v.pf.profile))
	}
	fields = append(fields,
		huh.NewSelect[string]().Title("Local model").
			Description("The route local means this model. Calibration measures it, and a model it has not measured gets the 60 seed requests.").
			Options(models...).Value(&v.pf.model),
		huh.NewInput().Title("Fallback routes").
			Description("Tried in order when the chosen route fails, separated by spaces, such as: local orcarouter/<model>").
			Value(&v.pf.fallback),
		huh.NewInput().Title("Local wait (seconds)").
			Description("How long a request waits for the local model before the next route. Empty: 30.").
			Value(&v.pf.wait).Validate(validInt(0, 3600)),
	)
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) profileDeleteForm() tea.Cmd {
	v.mode, v.confirm = "profiledel", false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete profile " + v.pf.profile + "?").
			Description("Clients that ask for it get an error.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// The rule forms.

func whenText(w router.When) string {
	var parts []string
	flag := func(name string, b *bool) {
		if b != nil {
			parts = append(parts, fmt.Sprintf("%s=%t", name, *b))
		}
	}
	flag("sensitive", w.Sensitive)
	flag("images", w.HasImages)
	flag("tools", w.HasTools)
	flag("json_schema", w.JSONSchema)
	flag("stream", w.Stream)
	flag("local_ready", w.LocalReady)
	flag("needs_provider", w.NeedsProvider)
	if w.PromptTokensOver != nil {
		parts = append(parts, "prompt>"+w.PromptTokensOver.String())
	}
	if w.MaxTokensOver != nil {
		parts = append(parts, fmt.Sprintf("max_tokens>%d", *w.MaxTokensOver))
	}
	for _, l := range []struct {
		name string
		vals []string
	}{{"task", w.Task}, {"difficulty", w.Difficulty}, {"hint", w.Hint}, {"clients", w.Clients}} {
		if len(l.vals) > 0 {
			parts = append(parts, l.name+"="+strings.Join(l.vals, ","))
		}
	}
	if len(parts) == 0 {
		return "always"
	}
	return strings.Join(parts, " ")
}

func (v *routerView) rulePickForm() tea.Cmd {
	var opts []huh.Option[string]
	for _, n := range v.profileNames() {
		p := v.r.Profiles.Profiles[n]
		for i, r := range p.Rules {
			opts = append(opts, huh.NewOption(fmt.Sprintf("%s %d. %s · %s → %s", n, i+1, r.Name, whenText(r.When), r.Route),
				n+"\x00"+strconv.Itoa(i)))
		}
		opts = append(opts, huh.NewOption(n+": add a rule", n+"\x00"+addRule))
	}
	if len(opts) == 0 {
		return note(false, "make a profile first: press m")
	}
	v.mode, v.pf.pick, v.pf.action = "rulepick", opts[0].Value, "edit"
	v.form = huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title("Rule").
			Description("The first rule that matches a request picks its route.").
			Options(opts...).Height(14).Value(&v.pf.pick)),
		huh.NewGroup(huh.NewSelect[string]().Title("Change").Options(
			huh.NewOption("Edit", "edit"),
			huh.NewOption("Move up", "up"),
			huh.NewOption("Move down", "down"),
			huh.NewOption("Delete", "delete"),
		).Value(&v.pf.action)).WithHideFunc(func() bool { return strings.HasSuffix(v.pf.pick, "\x00"+addRule) }),
	).WithShowHelp(false)
	return v.form.Init()
}

func triOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("any", anyValue), huh.NewOption("yes", "true"), huh.NewOption("no", "false"),
	}
}

func triOf(b *bool) string {
	if b == nil {
		return anyValue
	}
	return strconv.FormatBool(*b)
}

func boolOf(s string) *bool {
	if s == anyValue {
		return nil
	}
	b := s == "true"
	return &b
}

func labelOptions(labels []string) []huh.Option[string] {
	var opts []huh.Option[string]
	for _, l := range labels {
		if l != "unknown" {
			opts = append(opts, huh.NewOption(l, l))
		}
	}
	return opts
}

// pickedRule splits the rule pick into the profile and the rule index. The
// index is -1 for a new rule.
func (v *routerView) pickedRule() (string, int) {
	prof, idx, _ := strings.Cut(v.pf.pick, "\x00")
	if idx == addRule {
		return prof, -1
	}
	i, err := strconv.Atoi(idx)
	if err != nil {
		return prof, -1
	}
	return prof, i
}

func (v *routerView) ruleForm() tea.Cmd {
	prof, i := v.pickedRule()
	var r router.Rule
	if i >= 0 && i < len(v.r.Profiles.Profiles[prof].Rules) {
		r = v.r.Profiles.Profiles[prof].Rules[i]
	} else {
		r.Route = "local"
	}
	w := r.When
	v.mode = "rule"
	v.pf.ruleName, v.pf.route = r.Name, r.Route
	v.pf.sensitive, v.pf.needs = triOf(w.Sensitive), triOf(w.NeedsProvider)
	v.pf.images, v.pf.tools = triOf(w.HasImages), triOf(w.HasTools)
	v.pf.promptOver = ""
	if w.PromptTokensOver != nil {
		v.pf.promptOver = w.PromptTokensOver.String()
	}
	v.pf.tasks, v.pf.diffs, v.pf.clients = slices.Clone(w.Task), slices.Clone(w.Difficulty), slices.Clone(w.Clients)
	var clients []huh.Option[string]
	for _, c := range v.r.Clients {
		clients = append(clients, huh.NewOption(c.Name, c.Name))
	}
	keep := "The conditions that this form does not show are kept."
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Rule in profile "+prof).Description(keep),
			huh.NewInput().Title("Name").Value(&v.pf.ruleName).Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("a rule needs a name")
				}
				return nil
			}),
			huh.NewSelect[string]().Title("Route").
				Description("A local-only client never leaves the host, whatever the route.").
				Options(v.routeOptions(r.Route)...).Value(&v.pf.route),
			huh.NewSelect[string]().Title("Sensitive").Description("A secret or one of the sensitive words is in the request.").
				Options(triOptions()...).Value(&v.pf.sensitive),
			huh.NewInput().Title("Prompt longer than (tokens)").
				Description("A number, local_context for the local model's context, or empty for any.").
				Value(&v.pf.promptOver).Validate(func(s string) error {
				var t router.Tokens
				if s = strings.TrimSpace(s); s == "" {
					return nil
				}
				return t.UnmarshalJSON([]byte(strconv.Quote(s)))
			}),
		),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Task").Description("None chosen: any task.").
				Options(labelOptions(router.Tasks)...).Value(&v.pf.tasks),
			huh.NewMultiSelect[string]().Title("Difficulty").Description("None chosen: any difficulty.").
				Options(labelOptions(router.Difficulties)...).Value(&v.pf.diffs),
			huh.NewSelect[string]().Title("Needs a provider").
				Description("The calibration vote. It matches nothing until the local model has 20 judged requests.").
				Options(triOptions()...).Value(&v.pf.needs),
		),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Has images").Options(triOptions()...).Value(&v.pf.images),
			huh.NewSelect[string]().Title("Has tools").Options(triOptions()...).Value(&v.pf.tools),
			huh.NewMultiSelect[string]().Title("Clients").Description("None chosen: any client.").
				Options(clients...).Value(&v.pf.clients),
		),
	).WithShowHelp(false)
	return v.form.Init()
}

// ruleFromForm builds the rule from the form, over the conditions of the
// rule it replaces.
func (v *routerView) ruleFromForm(old router.When) (router.Rule, error) {
	w := old
	w.Sensitive, w.NeedsProvider = boolOf(v.pf.sensitive), boolOf(v.pf.needs)
	w.HasImages, w.HasTools = boolOf(v.pf.images), boolOf(v.pf.tools)
	w.Task, w.Difficulty, w.Clients = v.pf.tasks, v.pf.diffs, v.pf.clients
	w.PromptTokensOver = nil
	if s := strings.TrimSpace(v.pf.promptOver); s != "" {
		var t router.Tokens
		if err := t.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
			return router.Rule{}, err
		}
		w.PromptTokensOver = &t
	}
	return router.Rule{Name: strings.TrimSpace(v.pf.ruleName), When: w, Route: v.pf.route}, nil
}

// The calibration and classifier forms.

func (v *routerView) calibrationForm() tea.Cmd {
	cc := v.r.Profiles.Calibration
	v.mode = "calibration"
	v.pf.rate = strconv.FormatFloat(router.DefaultSampleRate*100, 'f', -1, 64)
	if cc.SampleRate != nil {
		v.pf.rate = strconv.FormatFloat(*cc.SampleRate*100, 'f', -1, 64)
	}
	v.pf.daily = strconv.FormatFloat(cc.Daily(), 'f', -1, 64)
	v.pf.judge, v.pf.compare = cc.Judge, cc.Compare
	v.pf.keepDays, v.pf.maxSamples = itoa(cc.KeepDays), itoa(cc.MaxSamples)
	v.pf.seed, v.pf.autoApply = !cc.NoSeed, cc.AutoApply
	providers := []huh.Option[string]{}
	for _, o := range v.routeOptions(cc.Compare) {
		if o.Value != "local" {
			providers = append(providers, o)
		}
	}
	judges := append([]huh.Option[string]{
		huh.NewOption("default: the listed provider model with the highest output price", ""),
		huh.NewOption("local: the profile's local model, which favours its own answers", "local"),
	}, providers...)
	compares := append([]huh.Option[string]{huh.NewOption("default: the profile's first provider route", "")}, providers...)
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Calibration").
				Description("Clients with Calibrate on have a share of their requests to a profile compared with the other side, in idle time."),
			huh.NewInput().Title("Sample rate (%)").Description("The share of a calibrating client's requests that is kept.").
				Value(&v.pf.rate).Validate(func(s string) error {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil || f < 0 || f > 100 {
					return errors.New("a percentage from 0 to 100")
				}
				return nil
			}),
			huh.NewInput().Title("Daily provider budget (US$)").
				Description("For the comparisons and the judge, per UTC day. 0 stops all provider calls.").
				Value(&v.pf.daily).Validate(func(s string) error {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil || f < 0 {
					return errors.New("a dollar amount, such as 1 or 0.50")
				}
				return nil
			}),
			huh.NewSelect[string]().Title("Judge").Options(judges...).Value(&v.pf.judge),
			huh.NewSelect[string]().Title("Compare with").
				Description("The provider that answers a request that the local model served.").
				Options(compares...).Value(&v.pf.compare),
		),
		huh.NewGroup(
			huh.NewInput().Title("Keep samples (days)").Description("Empty: 14.").
				Value(&v.pf.keepDays).Validate(validInt(1, 365)),
			huh.NewInput().Title("Most samples kept").Description("The oldest go first. Empty: 2000.").
				Value(&v.pf.maxSamples).Validate(validInt(1, 100000)),
			huh.NewConfirm().Title("Measure a new local model").
				Description("A local model with no measurements gets the 60 labelled requests first.").
				Affirmative("Yes").Negative("No").Value(&v.pf.seed),
			huh.NewConfirm().Title("Apply proposals by themselves").
				Description("Only proposals whose route the profile already names. Each change is logged.").
				Affirmative("Yes").Negative("No, I apply them").Value(&v.pf.autoApply),
		),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) classifierForm() tea.Cmd {
	f := v.r.Profiles
	cc := f.Classifier
	v.mode = "classifier"
	v.pf.words = strings.Join(f.SensitiveWords, ", ")
	v.pf.clsModel, v.pf.k, v.pf.timeout, v.pf.useStarter = cc.Model, itoa(cc.K), itoa(cc.TimeoutMS), !cc.NoStarter
	models := append([]huh.Option[string]{huh.NewOption("default: the first embedding model", "")},
		v.modelOptions(true, cc.Model)...)
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Sensitive words").
			Description("Separated by commas. A request with one of them is sensitive, as is one with a secret.").
			Value(&v.pf.words),
		huh.NewSelect[string]().Title("Classifier embedding model").Options(models...).Value(&v.pf.clsModel),
		huh.NewInput().Title("Voting examples (k)").Description("Empty: 5.").
			Value(&v.pf.k).Validate(validInt(1, 50)),
		huh.NewInput().Title("Classifier timeout (ms)").Description("Empty: 300.").
			Value(&v.pf.timeout).Validate(validInt(1, 10000)),
		huh.NewConfirm().Title("Starter examples").Description("The 150 examples that ship with Sibyl, next to your own.").
			Affirmative("Use them").Negative("Only mine").Value(&v.pf.useStarter),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) clearForm() tea.Cmd {
	v.mode, v.confirm = "clear", false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete every calibration sample?").
			Description("The samples are the only message content that Sibyl keeps. The judged results and the needs_provider votes go with them.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// submitProfiles handles the forms of this file. It returns false for a
// mode that it does not handle.
func (v *routerView) submitProfiles() (tea.Cmd, bool) {
	switch v.mode {
	case "profilepick", "profile", "profiledel", "rulepick", "rule", "calibration", "classifier", "clear":
	default:
		return nil, false
	}
	if v.mode == "profilepick" {
		if v.pf.profile != newProfile && v.pf.action == "delete" {
			return v.profileDeleteForm(), true
		}
		return v.profileForm(), true
	}
	if v.mode == "rulepick" {
		_, i := v.pickedRule()
		if i < 0 || v.pf.action == "edit" {
			return v.ruleForm(), true
		}
	}
	if v.mode == "clear" {
		if !v.confirm {
			return note(false, "the samples are kept"), true
		}
		return v.run("delete the calibration samples", func(ctx context.Context) (string, error) {
			_, err := v.c.ClearCalibration(ctx)
			return "", err
		}), true
	}
	f, err := v.profilesCopy()
	if err != nil {
		return note(false, "profiles: %v", err), true
	}
	what, err := v.changeProfiles(&f)
	switch {
	case err != nil:
		return note(false, "%v", err), true
	case what == "":
		return note(false, "nothing changed"), true
	}
	return v.saveProfiles(what, f), true
}

// changeProfiles applies the submitted form to f and names the change.
func (v *routerView) changeProfiles(f *router.ProfilesFile) (string, error) {
	switch v.mode {
	case "profile":
		name := v.pf.profile
		p := f.Profiles[name]
		if name == newProfile {
			name, p = strings.TrimSpace(v.pf.name), router.Profile{}
		}
		p.LocalModel, p.LocalWaitSeconds = v.pf.model, atoi(v.pf.wait)
		p.Fallback = strings.Fields(v.pf.fallback)
		f.Profiles[name] = p
		return "save profile " + name, nil
	case "profiledel":
		if !v.confirm {
			return "", nil
		}
		delete(f.Profiles, v.pf.profile)
		return "delete profile " + v.pf.profile, nil
	case "rulepick":
		prof, i := v.pickedRule()
		p := f.Profiles[prof]
		if i >= len(p.Rules) {
			return "", errors.New("the rules changed: open the form again")
		}
		switch v.pf.action {
		case "delete":
			p.Rules = slices.Delete(p.Rules, i, i+1)
		case "up":
			if i == 0 {
				return "", nil
			}
			p.Rules[i-1], p.Rules[i] = p.Rules[i], p.Rules[i-1]
		case "down":
			if i == len(p.Rules)-1 {
				return "", nil
			}
			p.Rules[i+1], p.Rules[i] = p.Rules[i], p.Rules[i+1]
		}
		f.Profiles[prof] = p
		return v.pf.action + " rule in " + prof, nil
	case "rule":
		prof, i := v.pickedRule()
		p := f.Profiles[prof]
		var old router.When
		if i >= 0 && i < len(p.Rules) {
			old = p.Rules[i].When
		}
		r, err := v.ruleFromForm(old)
		if err != nil {
			return "", err
		}
		if i >= 0 && i < len(p.Rules) {
			p.Rules[i] = r
		} else {
			p.Rules = append(p.Rules, r)
		}
		f.Profiles[prof] = p
		return "save rule " + r.Name, nil
	case "calibration":
		cc := &f.Calibration
		rate, _ := strconv.ParseFloat(strings.TrimSpace(v.pf.rate), 64)
		daily, _ := strconv.ParseFloat(strings.TrimSpace(v.pf.daily), 64)
		rate /= 100
		cc.SampleRate, cc.DailyUSD = &rate, &daily
		cc.Judge, cc.Compare = v.pf.judge, v.pf.compare
		cc.KeepDays, cc.MaxSamples = atoi(v.pf.keepDays), atoi(v.pf.maxSamples)
		cc.NoSeed, cc.AutoApply = !v.pf.seed, v.pf.autoApply
		return "save the calibration settings", nil
	case "classifier":
		var words []string
		for _, w := range strings.Split(v.pf.words, ",") {
			if w = strings.TrimSpace(w); w != "" {
				words = append(words, w)
			}
		}
		f.SensitiveWords = words
		c := &f.Classifier
		c.Model, c.K, c.TimeoutMS, c.NoStarter = v.pf.clsModel, atoi(v.pf.k), atoi(v.pf.timeout), !v.pf.useStarter
		return "save the classifier settings", nil
	}
	return "", nil
}

// renderRules lists each profile with its rules.
func (v *routerView) renderRules(b *strings.Builder) {
	for _, n := range v.profileNames() {
		p := v.r.Profiles.Profiles[n]
		model := n
		if n != "auto" {
			model = "auto:" + n
		}
		fmt.Fprintf(b, "  %-14s local %s", model, p.LocalModel)
		if len(p.Fallback) > 0 {
			b.WriteString(mutedSty.Render(" · fallback " + strings.Join(p.Fallback, " ")))
		}
		b.WriteString("\n")
		for i, r := range p.Rules {
			fmt.Fprintf(b, "    %d. %-28s %s → %s\n", i+1, r.Name, mutedSty.Render(whenText(r.When)), r.Route)
		}
	}
}
