package tui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

// The Router screen has one section at a time. Each section is a list on
// the left and the details of the selected row on the right, or under the
// list on a narrow screen.
const (
	secClients = iota
	secProviders
	secProfiles
	secCalibration
	secActivity
)

var sectionNames = []string{"Clients", "Providers", "Profiles", "Calibration", "Activity"}

// profRow is one row of the Profiles section: a profile, or one of its
// rules.
type profRow struct {
	profile string
	rule    int // -1 for the profile itself
}

func (v *routerView) profRows() []profRow {
	var out []profRow
	for _, n := range v.profileNames() {
		out = append(out, profRow{n, -1})
		for i := range v.r.Profiles.Profiles[n].Rules {
			out = append(out, profRow{n, i})
		}
	}
	return out
}

// sectionLen is the number of rows in the current section.
func (v *routerView) sectionLen() int {
	switch v.section {
	case secClients:
		return len(v.r.Clients)
	case secProviders:
		return len(v.r.Providers)
	case secProfiles:
		return len(v.profRows())
	}
	return 0
}

// cur is the cursor of the current section.
func (v *routerView) cur() *int {
	switch v.section {
	case secProviders:
		return &v.pcur
	case secProfiles:
		return &v.fcur
	}
	return &v.cursor
}

// waitingModel is the model that the most calibration samples wait for, if
// the worker waits for a model.
var waitingRE = regexp.MustCompile(`(?:\d+ sample\(s\): )?(?:the judge )?(\S+) is not loaded`)

func (v *routerView) waitingModel() string {
	st := v.cal.Status
	if st == nil || st.State != "waiting" {
		return ""
	}
	if m := waitingRE.FindStringSubmatch(st.Reason); m != nil {
		return m[1]
	}
	return ""
}

func (v *routerView) help() string {
	if v.form != nil {
		return formHelp(v.saveable())
	}
	h := "←/→ section"
	if v.key != "" {
		h = "y copy the key · esc hide the key · " + h
	}
	if v.sectionLen() > 0 {
		h += " · ↑/↓ select"
	}
	switch v.section {
	case secClients:
		h += " · n new client"
		if c, ok := v.selected(); ok {
			h += " · e edit"
			if !c.Builtin {
				h += " · K new key · d delete"
			}
		}
	case secProviders:
		h += " · n add a provider"
		if v.pcur < len(v.r.Providers) {
			h += " · e change · d delete"
		}
	case secProfiles:
		h += " · n new profile"
		rows := v.profRows()
		if v.fcur < len(rows) {
			r := rows[v.fcur]
			if r.rule < 0 {
				h += " · e edit the profile · a add a rule · d delete the profile"
			} else {
				h += " · e edit the rule · a add a rule · < move up · > move down · d delete the rule"
			}
		}
		h += " · s classifier"
	case secCalibration:
		h += " · e settings"
		if m := v.waitingModel(); m != "" {
			h += " · l load " + m
		}
		if len(v.cal.Proposals) > 0 {
			h += " · a apply a proposal"
		}
		if v.cal.Samples > 0 {
			h += " · C delete the samples"
		}
	}
	return h
}

func (v *routerView) keyPress(k string) tea.Cmd {
	switch k {
	case "y":
		if v.key != "" {
			return tea.Batch(tea.SetClipboard(v.key),
				note(true, "key for %s sent to the clipboard. If your terminal blocks that, drag to select it", v.keyFor))
		}
	case "esc":
		if v.key != "" {
			v.key, v.keyFor = "", ""
			return note(true, "key hidden")
		}
	case "left", "h":
		v.section = (v.section + len(sectionNames) - 1) % len(sectionNames)
		return nil
	case "right", "l":
		// l loads the waiting model on the Calibration section.
		if k == "l" && v.section == secCalibration && v.waitingModel() != "" {
			return v.loadWaiting()
		}
		v.section = (v.section + 1) % len(sectionNames)
		return nil
	case "up", "k":
		if c := v.cur(); *c > 0 {
			*c--
		}
		return nil
	case "down", "j":
		if c := v.cur(); *c < v.sectionLen()-1 {
			*c++
		}
		return nil
	}
	switch v.section {
	case secClients:
		return v.clientKey(k)
	case secProviders:
		return v.providerKey(k)
	case secProfiles:
		return v.profileKey(k)
	case secCalibration:
		return v.calibrationKey(k)
	}
	return nil
}

func (v *routerView) clientKey(k string) tea.Cmd {
	c, ok := v.selected()
	switch k {
	case "n":
		return v.newForm()
	case "e", "enter":
		if ok {
			return v.editForm(c)
		}
	case "d":
		if ok && !c.Builtin {
			return v.deleteForm(c.Name)
		}
	case "K":
		if ok && !c.Builtin {
			return v.rekeyForm(c.Name)
		}
	}
	return nil
}

func (v *routerView) providerKey(k string) tea.Cmd {
	switch k {
	case "n":
		return v.loadCatalog()
	case "e", "enter":
		if v.pcur < len(v.r.Providers) {
			p := v.r.Providers[v.pcur]
			return v.providerForm(&p)
		}
	case "d":
		if v.pcur < len(v.r.Providers) {
			return v.unproviderForm(v.r.Providers[v.pcur].Name)
		}
	}
	return nil
}

func (v *routerView) profileKey(k string) tea.Cmd {
	rows := v.profRows()
	var r profRow
	ok := v.fcur < len(rows)
	if ok {
		r = rows[v.fcur]
	}
	switch k {
	case "n":
		v.pf.profile = newProfile
		return v.loadModels("profile")
	case "s":
		return v.loadModels("classifier")
	}
	if !ok {
		return nil
	}
	pick := r.profile + "\x00" + strconv.Itoa(r.rule)
	switch k {
	case "e", "enter":
		if r.rule < 0 {
			v.pf.profile = r.profile
			return v.loadModels("profile")
		}
		v.pf.pick = pick
		return v.ruleForm()
	case "a":
		v.pf.pick = r.profile + "\x00" + addRule
		return v.ruleForm()
	case "d":
		if r.rule < 0 {
			v.pf.profile = r.profile
			return v.profileDeleteForm()
		}
		v.pf.pick = pick
		return v.ruleDeleteForm(r)
	case "<", ">":
		if r.rule < 0 {
			return nil
		}
		v.mode, v.pf.pick, v.pf.action = "rulepick", pick, "up"
		if k == ">" {
			v.pf.action = "down"
		}
		n := len(v.r.Profiles.Profiles[r.profile].Rules)
		switch {
		case k == "<" && r.rule > 0:
			v.fcur--
		case k == ">" && r.rule < n-1:
			v.fcur++
		}
		cmd, _ := v.submitProfiles()
		return cmd
	}
	return nil
}

func (v *routerView) calibrationKey(k string) tea.Cmd {
	switch k {
	case "e", "enter":
		return v.calibrationForm()
	case "a":
		if len(v.cal.Proposals) > 0 {
			return v.applyForm()
		}
	case "C":
		if v.cal.Samples > 0 {
			return v.clearForm()
		}
	}
	return nil
}

// loadWaiting loads the model that the calibration samples wait for.
func (v *routerView) loadWaiting() tea.Cmd {
	m := v.waitingModel()
	return v.run("load "+m, func(ctx context.Context) (string, error) { return "", v.c.LoadModel(ctx, m) })
}

// ruleDeleteForm asks before a rule is deleted.
func (v *routerView) ruleDeleteForm(r profRow) tea.Cmd {
	rule := v.r.Profiles.Profiles[r.profile].Rules[r.rule]
	v.mode, v.confirm = "ruledel", false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(fmt.Sprintf("Delete rule %d of %s, %q?", r.rule+1, r.profile, rule.Name)).
			Description("The next rules move up.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// render draws the section bar, then the current section.
// shownSecret is the new client key while it shows.
func (v *routerView) shownSecret() string { return v.key }

func (v *routerView) render(width int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	v.rows.reset()
	v.secZones = v.secZones[:0]
	if v.err != nil {
		b.WriteString(badSty.Render("API error: "+v.err.Error()) + "\n\n")
	}
	line := strings.Count(b.String(), "\n")
	x := 0
	for i, n := range sectionNames {
		if i > 0 {
			b.WriteString(mutedSty.Render(" │ "))
			x += 3
		}
		if i == v.section {
			b.WriteString(tabOn.Render(n))
		} else {
			b.WriteString(tabOff.Render(n))
		}
		w := ansi.StringWidth(n)
		v.secZones = append(v.secZones, zone{x0: x, x1: x + w, y: line, tab: i})
		x += w
	}
	b.WriteString("\n\n")
	if v.key != "" {
		b.WriteString(badSty.Render("Key for "+v.keyFor+". Copy it now, it is not shown again:") + "\n")
		// The key has a line of its own, so a double click or a drag
		// selects only the key.
		b.WriteString(selSty.Render(v.key) + "\n")
		b.WriteString(mutedSty.Render("The mouse is off while the key shows, so a drag selects it. y copies it, esc hides it.") + "\n\n")
	}
	top := strings.Count(b.String(), "\n")
	var list, details string
	switch v.section {
	case secClients:
		list, details = v.clientsList(top), v.clientDetails()
	case secProviders:
		list, details = v.providersList(top), v.providerDetails()
	case secProfiles:
		list, details = v.profilesList(top), v.profileDetails()
	case secCalibration:
		b.WriteString(v.calibrationPane())
		return b.String()
	case secActivity:
		b.WriteString(v.activityPane())
		return b.String()
	}
	b.WriteString(twoPane(list, details, width))
	return b.String()
}

// rowLine marks a list row for clicks. top is the line of the screen view
// where the list starts.
func (v *routerView) rowLine(b *strings.Builder, top, row int) {
	v.rows.lines[top+strings.Count(b.String(), "\n")] = row
}

func (v *routerView) clientsList(top int) string {
	var b strings.Builder
	if len(v.r.Clients) == 0 {
		b.WriteString(mutedSty.Render("  no clients · press n") + "\n")
	}
	for i, c := range v.r.Clients {
		v.rowLine(&b, top, i)
		b.WriteString(cursorLine(i == v.cursor, fmt.Sprintf("%-16s %-10s %5d req", c.Name, c.Policy, v.st.ByClient[c.Name])) + "\n")
	}
	return b.String()
}

func (v *routerView) clientDetails() string {
	c, ok := v.selected()
	if !ok {
		return mutedSty.Render("A client is a program with its own key, such as a laptop or Home Assistant.")
	}
	var b strings.Builder
	b.WriteString(titleSty.Render(c.Name) + "\n")
	policy := map[string]string{
		"local-only": "every request stays on this host",
		"hybrid":     "a profile can send a request to a provider",
	}[c.Policy]
	fmt.Fprintf(&b, "Policy     %s  %s\n", c.Policy, mutedSty.Render(policy))
	fmt.Fprintf(&b, "Requests   %d\n", v.st.ByClient[c.Name])
	fmt.Fprintf(&b, "Spend      %s\n", budgetText(c, v.st.Spend[c.Name]))
	if c.Policy == "hybrid" {
		fmt.Fprintf(&b, "Cloud hint %s  %s\n", yesNo(c.AllowHint), mutedSty.Render("X-Sibyl-Route: cloud"))
		fmt.Fprintf(&b, "Calibrate  %s\n", yesNo(c.Calibrate))
	}
	if c.Builtin {
		b.WriteString("\n" + mutedSty.Render("Built in. The default key is in /var/lib/sibyl-operator/inference.key.") + "\n")
	}
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (v *routerView) providersList(top int) string {
	var b strings.Builder
	if len(v.r.Providers) == 0 {
		b.WriteString(mutedSty.Render("  none · every request stays on this host · press n") + "\n")
	}
	for i, p := range v.r.Providers {
		key := goodSty.Render("key set")
		if !p.KeySet {
			key = badSty.Render("no key")
		}
		v.rowLine(&b, top, i)
		b.WriteString(cursorLine(i == v.pcur, fmt.Sprintf("%-16s %-9s %s", p.Name, p.Type, key)) + "\n")
	}
	return b.String()
}

func (v *routerView) providerDetails() string {
	if v.pcur >= len(v.r.Providers) {
		return mutedSty.Render("A provider is a cloud API that a hybrid client's profile can send a request to.")
	}
	p := v.r.Providers[v.pcur]
	var b strings.Builder
	b.WriteString(titleSty.Render(p.Name) + "\n")
	url := p.BaseURL
	if url == "" {
		url = "the API's default URL"
	}
	fmt.Fprintf(&b, "API      %s\nURL      %s\n", p.Type, url)
	if p.Catalog != "" {
		fmt.Fprintf(&b, "Index    models.dev %s\n", p.Catalog)
	}
	fmt.Fprintf(&b, "Route    %s\n", mutedSty.Render(p.Name+"/<model>"))
	if len(p.Models) == 0 {
		b.WriteString("Models   " + mutedSty.Render("none listed") + "\n")
	}
	for i, m := range p.Models {
		label := "         "
		if i == 0 {
			label = "Models   "
		}
		price := mutedSty.Render("no price")
		if pr, ok := p.Prices[m]; ok {
			price = fmt.Sprintf("$%g in, $%g out per Mtok", pr.Input, pr.Output)
		}
		fmt.Fprintf(&b, "%s%s  %s\n", label, m, price)
	}
	return b.String()
}

func (v *routerView) profilesList(top int) string {
	var b strings.Builder
	rows := v.profRows()
	if len(rows) == 0 {
		b.WriteString(mutedSty.Render("  no profiles · press n") + "\n")
	}
	for i, r := range rows {
		v.rowLine(&b, top, i)
		if r.rule < 0 {
			name := r.profile
			if name != "auto" {
				name = "auto:" + name
			}
			b.WriteString(cursorLine(i == v.fcur, name) + "\n")
			continue
		}
		rule := v.r.Profiles.Profiles[r.profile].Rules[r.rule]
		b.WriteString(cursorLine(i == v.fcur, fmt.Sprintf("  %d. %s → %s", r.rule+1, rule.Name, rule.Route)) + "\n")
	}
	return b.String()
}

func (v *routerView) profileDetails() string {
	rows := v.profRows()
	if v.fcur >= len(rows) {
		return mutedSty.Render("A profile is the model auto or auto:<name>. Its rules pick a route for each request.")
	}
	r := rows[v.fcur]
	p := v.r.Profiles.Profiles[r.profile]
	var b strings.Builder
	if r.rule >= 0 {
		rule := p.Rules[r.rule]
		fmt.Fprintf(&b, "%s\n", titleSty.Render(fmt.Sprintf("Rule %d of %s", r.rule+1, r.profile)))
		fmt.Fprintf(&b, "Name   %s\nWhen   %s\nRoute  %s\n", rule.Name, whenText(rule.When), rule.Route)
		if rule.Route == "local" {
			fmt.Fprintf(&b, "       %s\n", mutedSty.Render("local is "+p.LocalModel))
		}
		b.WriteString("\n" + mutedSty.Render("The first rule that matches a request picks its route.") + "\n")
		return b.String()
	}
	model := r.profile
	if model != "auto" {
		model = "auto:" + model
	}
	b.WriteString(titleSty.Render("Profile "+model) + "\n")
	fmt.Fprintf(&b, "Local model  %s\n", p.LocalModel)
	wait := p.LocalWaitSeconds
	if wait == 0 {
		wait = 30
	}
	fmt.Fprintf(&b, "Local wait   %ds\n", wait)
	fb := "none"
	if len(p.Fallback) > 0 {
		fb = strings.Join(p.Fallback, " ")
	}
	fmt.Fprintf(&b, "Fallback     %s\n", fb)
	fmt.Fprintf(&b, "Rules        %d\n", len(p.Rules))
	b.WriteString("\n" + mutedSty.Render("A client asks for it with the model "+model+". e changes the local model, the wait and the fallback.") + "\n")
	return b.String()
}

func (v *routerView) calibrationPane() string {
	c := v.cal
	var b strings.Builder
	b.WriteString(mutedSty.Render("Calibration keeps a share of a calibrating client's requests, and compares the local answer with a provider's, in idle time.") + "\n\n")
	fmt.Fprintf(&b, "Samples   %d: %d judged, %d waiting, %d failed\n", c.Samples, c.Judged, c.Pending, c.Failed)
	fmt.Fprintf(&b, "Budget    %s of %s today\n", money(c.SpentTodayUSD), money(c.DailyUSD))
	if st := c.Status; st != nil {
		state := st.State
		if st.Reason != "" {
			state += ": " + st.Reason
		}
		sty := mutedSty
		if st.State == "waiting" || st.State == "error" {
			sty = badSty
		}
		b.WriteString("Worker    " + sty.Render(state) + "\n")
		if m := v.waitingModel(); m != "" {
			b.WriteString("          " + mutedSty.Render("l loads "+m+", so its samples can run.") + "\n")
			b.WriteString("          " + mutedSty.Render("A sample keeps the model of its request. C deletes the samples.") + "\n")
		}
	}
	if len(c.Cells) > 0 {
		b.WriteString("\n" + titleSty.Render("Results") + "\n")
	}
	for _, cell := range c.Cells {
		rate := fmt.Sprintf("%3.0f%%", cell.LocalOKRate*100)
		if cell.Comparisons >= 50 {
			rate = selSty.Render(rate)
		}
		fmt.Fprintf(&b, "  %-10s %-7s local won or tied %s of %3d  local %5.1fs  provider %5.1fs  $%.4f\n",
			cell.Task, cell.Difficulty, rate, cell.Comparisons,
			float64(cell.LocalLatencyMS)/1000, float64(cell.ProviderLatencyMS)/1000, cell.ProviderCostUSD)
	}
	for _, p := range c.Proposals {
		b.WriteString("  " + selSty.Render("proposal") + " " + proposalText(p) + "\n")
	}
	return b.String()
}

func (v *routerView) activityPane() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d requests, %d errors", v.st.Requests, v.st.Errors)
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
	if v.st.Unpriced > 0 {
		b.WriteString(mutedSty.Render("A request with no price is not counted in a budget. Add prices to the provider's models.") + "\n")
	}
	b.WriteString("\n" + titleSty.Render("Last decisions") + "\n")
	for _, e := range v.st.Last {
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

// clickAt handles a click on the section bar or on a list row.
func (v *routerView) clickAt(line, x int) tea.Cmd {
	for _, z := range v.secZones {
		if z.y == line && x >= z.x0 && x < z.x1 {
			v.section = z.tab
			return nil
		}
	}
	return v.rows.click(line, v.cur(), v.update)
}
