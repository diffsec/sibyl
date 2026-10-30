package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
)

type routerLoaded struct {
	r   client.Router
	st  client.RouterStats
	cal client.RouterCalibration
	err error
}

type routerDone struct {
	what string
	key  string // a new client's key
	err  error
}

type routerTick struct{}

// catalogLoaded carries the models.dev provider list for the picker.
type catalogLoaded struct {
	cat client.Catalog
	err error
}

// catalogModelsLoaded carries one provider's models for the picker.
type catalogModelsLoaded struct {
	p   client.CatalogModels
	err error
}

// customProvider is the picker's choice for a provider that models.dev
// does not list.
const customProvider = "\x00custom"

// routerView manages the model router: its clients, their keys, policies
// and budgets, and the providers. It shows the spend and the last
// decisions. Profiles are edited through PUT /system/router/profiles.
type routerView struct {
	c      *client.Client
	r      client.Router
	st     client.RouterStats
	cal    client.RouterCalibration
	err    error
	cursor int // selected client
	ticks  bool

	form *huh.Form
	mode string // new, edit, delete, rekey, pick, catalog, provider, unprovider, apply
	// Form values.
	name, policy, daily, monthly  string
	allowHint, calibrate, confirm bool
	ptype, baseURL, apiKey, mods  string
	// The models.dev picker: the chosen provider and models.
	pick     string
	catModel []string
	catProv  client.CatalogModels
	// proposal is the calibration proposal that the apply form picks.
	proposal string
	// pf holds the values of the profile, rule and calibration forms, and
	// models the installed models that they offer.
	pf     profileForm
	models []client.Model
	// key is a new client's key, shown until the next action.
	key, keyFor string
}

func newRouterView(c *client.Client) *routerView { return &routerView{c: c} }

func (v *routerView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		r, err := v.c.Router(ctx)
		if err != nil {
			return routerLoaded{err: err}
		}
		st, err := v.c.RouterStats(ctx)
		if err != nil {
			return routerLoaded{r: r, st: st, err: err}
		}
		// An older System API has no calibration report.
		cal, _ := v.c.RouterCalibration(ctx)
		return routerLoaded{r: r, st: st, cal: cal}
	}
}

func (v *routerView) activate() tea.Cmd {
	if v.ticks {
		return v.fetch()
	}
	v.ticks = true
	return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return routerTick{} }))
}

func (v *routerView) capturing() bool { return v.form != nil }

func (v *routerView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	h := "n new client · e edit · p add or change provider · x delete provider · m profiles · r rules · c calibration · s classifier · ↑/↓ select"
	if len(v.cal.Proposals) > 0 {
		h += " · a apply a calibration proposal"
	}
	if v.cal.Samples > 0 {
		h += " · C delete the samples"
	}
	if c, ok := v.selected(); ok && !c.Builtin {
		h += " · k new key · d delete client"
	}
	return h
}

func (v *routerView) selected() (client.RouterClient, bool) {
	if v.cursor < 0 || v.cursor >= len(v.r.Clients) {
		return client.RouterClient{}, false
	}
	return v.r.Clients[v.cursor], true
}

var clientNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validName(s string) error {
	if !clientNameRE.MatchString(strings.TrimSpace(s)) {
		return errors.New("1 to 32 lowercase letters, digits and dashes")
	}
	return nil
}

func validUSD(s string) error {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err != nil || f < 0 {
		return errors.New("a dollar amount, such as 5 or 2.50, or empty for no limit")
	}
	return nil
}

func policyOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("local-only: requests never leave this host", "local-only"),
		huh.NewOption("hybrid: the routing rules can send requests to a provider", "hybrid"),
	}
}

func budgetFields(daily, monthly *string) []huh.Field {
	return []huh.Field{
		huh.NewInput().Title("Daily budget (US$)").Description("Provider spend per UTC day. Empty: no limit.").
			Value(daily).Validate(validUSD),
		huh.NewInput().Title("Monthly budget (US$)").Description("Provider spend per UTC month. Empty: no limit.").
			Value(monthly).Validate(validUSD),
	}
}

func (v *routerView) newForm() tea.Cmd {
	v.mode, v.name, v.policy, v.daily, v.monthly = "new", "", "local-only", "", ""
	fields := append([]huh.Field{
		huh.NewInput().Title("Client name").Description("For example laptop or home-assistant.").
			Value(&v.name).Validate(validName),
		huh.NewSelect[string]().Title("Policy").Options(policyOptions()...).Value(&v.policy),
	}, budgetFields(&v.daily, &v.monthly)...)
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func usd(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func (v *routerView) editForm(c client.RouterClient) tea.Cmd {
	v.mode, v.name, v.policy, v.allowHint, v.calibrate = "edit", c.Name, c.Policy, c.AllowHint, c.Calibrate
	v.daily, v.monthly = "", ""
	if c.Budget != nil {
		v.daily, v.monthly = usd(c.Budget.DailyUSD), usd(c.Budget.MonthlyUSD)
	}
	fields := append([]huh.Field{
		huh.NewNote().Title("Client " + c.Name),
		huh.NewSelect[string]().Title("Policy").Options(policyOptions()...).Value(&v.policy),
		huh.NewConfirm().Title("Allow the cloud hint").
			Description("Lets the client send X-Sibyl-Route: cloud to ask for a provider.").
			Affirmative("Allow").Negative("No").Value(&v.allowHint),
		huh.NewConfirm().Title("Calibrate").
			Description("Keeps 2% of the requests and answers on the host to compare the local model with a provider. Hybrid clients only.").
			Affirmative("Yes").Negative("No").Value(&v.calibrate),
	}, budgetFields(&v.daily, &v.monthly)...)
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) deleteForm(name string) tea.Cmd {
	v.mode, v.name, v.confirm = "delete", name, false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete client " + name + "?").
			Description("Its key stops working at once.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) rekeyForm(name string) tea.Cmd {
	v.mode, v.name, v.confirm = "rekey", name, false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("New key for " + name + "?").
			Description("The old key stops working at once. The new key is shown once.").
			Affirmative("New key").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// loadCatalog starts the models.dev picker.
func (v *routerView) loadCatalog() tea.Cmd {
	return call(v.c.Catalog, func(c client.Catalog, err error) tea.Msg { return catalogLoaded{c, err} })
}

// pickForm lists the models.dev providers that the router can use.
func (v *routerView) pickForm(cat client.Catalog) tea.Cmd {
	if len(cat.Providers) == 0 {
		return v.providerForm()
	}
	opts := []huh.Option[string]{huh.NewOption("Other: enter the API and the base URL", customProvider)}
	for _, p := range cat.Providers {
		api := "OpenAI API"
		if p.Type == "anthropic" {
			api = "Anthropic API"
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s (%s) · %d models · %s", p.Name, p.ID, p.Models, api), p.ID))
	}
	v.mode, v.pick = "pick", customProvider
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Provider").
			Description(fmt.Sprintf("From the models.dev index of %s. Press / to search.", strings.SplitN(cat.Fetched, "T", 2)[0])).
			Options(opts...).Height(14).Value(&v.pick),
	)).WithShowHelp(false)
	return v.form.Init()
}

// providerName turns a models.dev ID into a provider name for the router.
func providerName(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	n := strings.Trim(b.String(), "-")
	if len(n) > 32 {
		n = strings.Trim(n[:32], "-")
	}
	return n
}

func modelLabel(m client.CatalogModel) string {
	l := m.ID
	if m.Context > 0 {
		l += fmt.Sprintf("  %dk ctx", m.Context/1000)
	}
	if m.Cost != nil {
		l += fmt.Sprintf("  $%g/$%g per M", m.Cost.Input, m.Cost.Output)
	} else {
		l += "  no price"
	}
	if !m.ToolCall {
		l += "  no tools"
	}
	return l
}

// catalogForm sets up a provider from models.dev: the name, the key, and
// the models to list. The API and base URL come from the index.
func (v *routerView) catalogForm(p client.CatalogModels) tea.Cmd {
	v.mode, v.catProv = "catalog", p
	v.name, v.apiKey, v.catModel = providerName(p.ID), "", nil
	for _, rp := range v.r.Providers {
		if rp.Name == v.name {
			v.catModel = append(v.catModel, rp.Models...)
		}
	}
	var opts []huh.Option[string]
	for _, m := range p.Models {
		opts = append(opts, huh.NewOption(modelLabel(m), m.ID))
	}
	keyNote := "Sibyl checks the key with the provider before it stores it."
	if len(p.Env) > 0 {
		keyNote = "The key that tools read from " + strings.Join(p.Env, " or ") + ". " + keyNote
	}
	// The provider details go in a description: a note as the first field
	// leaves the form blank until the first key.
	about := p.Name + " · " + p.BaseURL
	if p.Doc != "" {
		about += " · " + p.Doc
	}
	fields := []huh.Field{
		huh.NewInput().Title("Provider name").
			Description(about + "\nClients name its models as <provider>/<model>.").
			Value(&v.name).Validate(validName),
		huh.NewInput().Title("API key").Description(keyNote).EchoMode(huh.EchoModePassword).Value(&v.apiKey),
	}
	if len(opts) > 0 {
		fields = append(fields, huh.NewMultiSelect[string]().Title("Models to list").
			Description("GET /v1/models lists them for hybrid clients. Press / to search, space to choose.").
			Options(opts...).Filterable(true).Height(12).Value(&v.catModel))
	}
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) providerForm() tea.Cmd {
	v.mode, v.name, v.ptype, v.baseURL, v.apiKey, v.mods = "provider", "", "openai", "", "", ""
	known := map[string]client.RouterProvider{}
	for _, p := range v.r.Providers {
		known[p.Name] = p
	}
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Provider name").
				Description("Clients name its models as <provider>/<model>. A known name changes that provider.").
				Value(&v.name).Validate(func(s string) error {
				if s == "local" || s == "auto" {
					return errors.New("local and auto are reserved")
				}
				return validName(s)
			}),
			huh.NewSelect[string]().Title("API").Options(
				huh.NewOption("OpenAI-compatible: OpenAI, OpenRouter and similar", "openai"),
				huh.NewOption("Anthropic Messages API", "anthropic"),
			).Value(&v.ptype),
		),
		huh.NewGroup(
			huh.NewInput().Title("Base URL").DescriptionFunc(func() string {
				if v.ptype == "anthropic" {
					return "Empty for https://api.anthropic.com."
				}
				return "Up to the endpoint path, such as https://api.openai.com/v1 or https://openrouter.ai/api/v1."
			}, &v.ptype).Value(&v.baseURL),
			huh.NewInput().Title("API key").DescriptionFunc(func() string {
				if _, ok := known[strings.TrimSpace(v.name)]; ok {
					return "Empty keeps the stored key. Sibyl checks a new key with the provider."
				}
				return "Sibyl checks the key with the provider before it stores it."
			}, &v.name).EchoMode(huh.EchoModePassword).Value(&v.apiKey),
			huh.NewInput().Title("Models to list").
				Description("Optional, separated by spaces. GET /v1/models lists them for hybrid clients.").
				Value(&v.mods),
		),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *routerView) unproviderForm() tea.Cmd {
	var opts []huh.Option[string]
	for _, p := range v.r.Providers {
		opts = append(opts, huh.NewOption(p.Name+"  "+p.Type, p.Name))
	}
	if len(opts) == 0 {
		return note(false, "there are no providers")
	}
	v.mode, v.name, v.confirm = "unprovider", opts[0].Value, false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Provider to delete").Options(opts...).Value(&v.name),
		huh.NewConfirm().Title("Delete it?").Description("Its key is deleted too. A profile that names it must change first.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// budget reads the budget fields. It returns nil when both are empty.
func (v *routerView) budget() *client.RouterBudget {
	d, _ := strconv.ParseFloat(strings.TrimSpace(v.daily), 64)
	m, _ := strconv.ParseFloat(strings.TrimSpace(v.monthly), 64)
	if d <= 0 && m <= 0 {
		return nil
	}
	return &client.RouterBudget{DailyUSD: d, MonthlyUSD: m}
}

// run sends one change to the System API and reports the result.
func (v *routerView) run(what string, fn func(context.Context) (string, error)) tea.Cmd {
	v.key, v.keyFor = "", ""
	return tea.Batch(note(true, "%s…", what), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		key, err := fn(ctx)
		return routerDone{what, key, err}
	})
}

func (v *routerView) submit() tea.Cmd {
	if cmd, ok := v.submitProfiles(); ok {
		return cmd
	}
	run := v.run
	name, b := strings.TrimSpace(v.name), v.budget()
	switch v.mode {
	case "new":
		policy := v.policy
		return run("create client "+name, func(ctx context.Context) (string, error) {
			nc, err := v.c.CreateRouterClient(ctx, name, policy, b)
			return nc.Key, err
		})
	case "edit":
		policy, hint, calibrate := v.policy, v.allowHint, v.calibrate
		return run("change client "+name, func(ctx context.Context) (string, error) {
			return "", v.c.SetRouterClient(ctx, name, policy, hint, b, calibrate)
		})
	case "rekey":
		if !v.confirm {
			return note(false, "%s keeps its key", name)
		}
		return run("new key for "+name, func(ctx context.Context) (string, error) {
			nc, err := v.c.NewRouterClientKey(ctx, name)
			return nc.Key, err
		})
	case "apply":
		if !v.confirm {
			return note(false, "the rules are unchanged")
		}
		id := v.proposal
		return run("apply calibration proposal", func(ctx context.Context) (string, error) {
			return "", v.c.ApplyCalibration(ctx, id)
		})
	case "delete":
		if !v.confirm {
			return note(false, "%s kept", name)
		}
		return run("delete client "+name, func(ctx context.Context) (string, error) {
			return "", v.c.DeleteRouterClient(ctx, name)
		})
	case "pick":
		if v.pick == customProvider {
			return v.providerForm()
		}
		id := v.pick
		return tea.Batch(note(true, "loading the models of %s…", id),
			call(func(ctx context.Context) (client.CatalogModels, error) { return v.c.CatalogProvider(ctx, id) },
				func(p client.CatalogModels, err error) tea.Msg { return catalogModelsLoaded{p, err} }))
	case "catalog":
		p := client.ProviderRequest{Type: v.catProv.Type, BaseURL: v.catProv.BaseURL,
			APIKey: strings.TrimSpace(v.apiKey), Models: v.catModel, Catalog: v.catProv.ID}
		v.apiKey = ""
		return run("set provider "+name+" (checking the key)", func(ctx context.Context) (string, error) {
			return "", v.c.SetRouterProvider(ctx, name, p)
		})
	case "provider":
		p := client.ProviderRequest{Type: v.ptype, BaseURL: strings.TrimSpace(v.baseURL),
			APIKey: strings.TrimSpace(v.apiKey), Models: strings.Fields(v.mods)}
		v.apiKey = ""
		return run("set provider "+name+" (checking the key)", func(ctx context.Context) (string, error) {
			return "", v.c.SetRouterProvider(ctx, name, p)
		})
	case "unprovider":
		if !v.confirm {
			return note(false, "%s kept", name)
		}
		return run("delete provider "+name, func(ctx context.Context) (string, error) {
			return "", v.c.DeleteRouterProvider(ctx, name)
		})
	}
	return nil
}

func (v *routerView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case routerTick:
		return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return routerTick{} }))
	case routerLoaded:
		v.err = msg.err
		if msg.err == nil || len(msg.r.Clients) > 0 {
			v.r, v.st, v.cal = msg.r, msg.st, msg.cal
		}
		if v.cursor >= len(v.r.Clients) {
			v.cursor = 0
		}
		return nil
	case modelsForForm:
		return v.modelsLoaded(msg)
	case catalogLoaded:
		if msg.err != nil {
			// With no index, the picker falls back to the manual form.
			return tea.Batch(note(false, "models.dev index: %v", msg.err), v.providerForm())
		}
		return v.pickForm(msg.cat)
	case catalogModelsLoaded:
		if msg.err != nil {
			return note(false, "models of %s: %v", v.pick, msg.err)
		}
		return v.catalogForm(msg.p)
	case routerDone:
		if msg.err != nil {
			return tea.Batch(note(false, "%s: %v", msg.what, msg.err), v.fetch())
		}
		if msg.key != "" {
			v.key, v.keyFor = msg.key, strings.TrimPrefix(strings.TrimPrefix(msg.what, "create client "), "new key for ")
		}
		return tea.Batch(note(true, "%s: done", msg.what), v.fetch())
	case tea.KeyPressMsg:
		if v.form == nil {
			return v.keyPress(msg.String())
		}
		if msg.String() == "esc" {
			v.form, v.apiKey = nil, ""
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
		v.form, v.apiKey = nil, ""
	}
	return cmd
}

func (v *routerView) keyPress(k string) tea.Cmd {
	c, ok := v.selected()
	switch k {
	case "up":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < len(v.r.Clients)-1 {
			v.cursor++
		}
	case "n":
		return v.newForm()
	case "e":
		if ok {
			return v.editForm(c)
		}
	case "d":
		if ok && !c.Builtin {
			return v.deleteForm(c.Name)
		}
	case "k":
		if ok && !c.Builtin {
			return v.rekeyForm(c.Name)
		}
	case "p":
		return v.loadCatalog()
	case "x":
		return v.unproviderForm()
	case "a":
		if len(v.cal.Proposals) > 0 {
			return v.applyForm()
		}
	case "m":
		return v.loadModels("profilepick")
	case "r":
		return v.rulePickForm()
	case "c":
		return v.calibrationForm()
	case "s":
		return v.loadModels("classifier")
	case "C":
		if v.cal.Samples > 0 {
			return v.clearForm()
		}
	}
	return nil
}

func proposalText(p client.CalibrationProposal) string {
	return fmt.Sprintf("%s: %s %s from %s to %s (local won or tied %.0f%% of %d)",
		p.Profile, p.Task, p.Difficulty, p.Current, p.Route, p.LocalOKRate*100, p.Comparisons)
}

func (v *routerView) applyForm() tea.Cmd {
	v.mode, v.proposal, v.confirm = "apply", v.cal.Proposals[0].ID, false
	opts := make([]huh.Option[string], len(v.cal.Proposals))
	for i, p := range v.cal.Proposals {
		opts[i] = huh.NewOption(proposalText(p), p.ID)
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Calibration proposal").Options(opts...).Value(&v.proposal),
		huh.NewConfirm().Title("Add this rule to the profile?").
			Description("The rule goes after the rules for sensitive and long requests.").
			Affirmative("Apply").Negative("Keep the rules").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func money(f float64) string { return fmt.Sprintf("$%.2f", f) }

// budgetText shows a client's spend against its budget.
func budgetText(c client.RouterClient, s client.RouterSpend) string {
	day, month := money(s.Today)+" today", money(s.Month)+" this month"
	if c.Budget == nil {
		if c.Policy == "hybrid" {
			return day + ", " + month + "  " + badSty.Render("no budget")
		}
		return mutedSty.Render("stays on this host")
	}
	if c.Budget.DailyUSD > 0 {
		day += " of " + money(c.Budget.DailyUSD)
	}
	if c.Budget.MonthlyUSD > 0 {
		month += " of " + money(c.Budget.MonthlyUSD)
	}
	return day + ", " + month
}

func (v *routerView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.err != nil {
		b.WriteString(badSty.Render("API error: "+v.err.Error()) + "\n\n")
	}

	b.WriteString(titleSty.Render("Clients") + "\n")
	for i, c := range v.r.Clients {
		policy := mutedSty.Render(c.Policy)
		if c.Policy == "hybrid" {
			policy = selSty.Render(c.Policy)
		}
		line := fmt.Sprintf("%-16s %-10s %5d req  %s", c.Name, policy, v.st.ByClient[c.Name], budgetText(c, v.st.Spend[c.Name]))
		if c.Calibrate && c.Policy == "hybrid" {
			line += "  " + mutedSty.Render("calibrates")
		}
		b.WriteString(cursorLine(i == v.cursor, line) + "\n")
	}
	if v.key != "" {
		b.WriteString(badSty.Render("  Key for "+v.keyFor+". Copy it now, it is not shown again:") + "\n")
		b.WriteString("    " + selSty.Render(v.key) + "\n")
	}
	b.WriteString(mutedSty.Render("  The default key is in /var/lib/sibyl-operator/inference.key. Programs use their own keys.") + "\n")

	b.WriteString("\n" + titleSty.Render("Providers") + "\n")
	if len(v.r.Providers) == 0 {
		b.WriteString(mutedSty.Render("  none · every request stays on this host · press p to add one") + "\n")
	}
	for _, p := range v.r.Providers {
		key := goodSty.Render("key set")
		if !p.KeySet {
			key = badSty.Render("no key")
		}
		url := p.BaseURL
		if url == "" {
			url = "default URL"
		}
		fmt.Fprintf(&b, "  %-14s %-9s %s  %s\n", p.Name, p.Type, key, mutedSty.Render(url))
		if len(p.Models) > 0 {
			b.WriteString(mutedSty.Render("      "+strings.Join(p.Models, " ")) + "\n")
		}
	}

	b.WriteString("\n" + titleSty.Render("Profiles") + "\n")
	if len(v.r.Profiles.Profiles) == 0 {
		b.WriteString(mutedSty.Render("  none · press m to make one") + "\n")
	}
	v.renderRules(&b)

	v.renderCalibration(&b)

	fmt.Fprintf(&b, "\n%s  %d requests, %d errors", titleSty.Render("Last decisions"), v.st.Requests, v.st.Errors)
	if v.st.Unpriced > 0 {
		b.WriteString(badSty.Render(fmt.Sprintf(", %d with no price", v.st.Unpriced)))
	}
	regen := 0
	for _, n := range v.st.Regenerated {
		regen += n
	}
	if regen > 0 {
		fmt.Fprintf(&b, ", %d asked again within a minute", regen)
	}
	b.WriteString("\n")
	for i, e := range v.st.Last {
		if i == 8 {
			break
		}
		route := e.Route
		if route == "" {
			route = "refused"
		}
		line := fmt.Sprintf("  %s %-12s %-40s %3d", e.Time.Local().Format("15:04:05"), e.Client, route, e.Status)
		if e.CostUSD > 0 {
			line += fmt.Sprintf("  $%.4f", e.CostUSD)
		}
		if e.Rule != "" {
			line += "  " + mutedSty.Render(e.Rule)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderCalibration shows the calibration report, when there are samples.
func (v *routerView) renderCalibration(b *strings.Builder) {
	c := v.cal
	if c.Samples == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s  %d samples, %d judged, %d waiting, %d failed · %s of %s today\n",
		titleSty.Render("Calibration"), c.Samples, c.Judged, c.Pending, c.Failed, money(c.SpentTodayUSD), money(c.DailyUSD))
	if st := c.Status; st != nil && (st.State == "waiting" || st.State == "error") {
		b.WriteString(badSty.Render("  "+st.State+": "+st.Reason) + "\n")
	}
	for i, cell := range c.Cells {
		if i == 10 {
			b.WriteString(mutedSty.Render(fmt.Sprintf("  and %d more", len(c.Cells)-i)) + "\n")
			break
		}
		rate := fmt.Sprintf("%3.0f%%", cell.LocalOKRate*100)
		if cell.Comparisons >= 50 {
			rate = selSty.Render(rate)
		}
		fmt.Fprintf(b, "  %-10s %-7s local won or tied %s of %3d  local %5.1fs  provider %5.1fs  $%.4f\n",
			cell.Task, cell.Difficulty, rate, cell.Comparisons,
			float64(cell.LocalLatencyMS)/1000, float64(cell.ProviderLatencyMS)/1000, cell.ProviderCostUSD)
	}
	for _, p := range c.Proposals {
		b.WriteString("  " + selSty.Render("proposal") + " " + proposalText(p) + "\n")
	}
}
