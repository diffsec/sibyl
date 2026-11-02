package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/diag"
	"github.com/diffsec/sibyl/internal/hostd"
)

type diagLoaded struct {
	st      diag.StatusFile
	perf    diag.PerfFile
	err     error
	perfErr error
}

type diagDone struct {
	what string
	res  hostd.Result
	err  error
}

type diagTick struct{}

// diagView is the Diagnostics screen: the watchdog, speed per model,
// incidents and bundles. It makes and sends bundles through the System API,
// which passes them to sibyl-hostd, so the operator needs no sudo.
type diagView struct {
	c      *client.Client
	data   diagLoaded
	cursor int // selected incident
	ticks  bool

	form    *huh.Form
	mode    string // bundle, submit, consent, safe-mode
	source  string // selected or new
	note    string
	consent string
	confirm bool
}

func newDiagView(c *client.Client) *diagView { return &diagView{c: c} }

func (v *diagView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var m diagLoaded
		m.st, m.err = v.c.Diagnostics(ctx)
		m.perf, m.perfErr = v.c.DiagnosticsPerf(ctx)
		return m
	}
}

func (v *diagView) activate() tea.Cmd {
	if v.ticks {
		return v.fetch()
	}
	v.ticks = true
	return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return diagTick{} }))
}

func (v *diagView) capturing() bool { return v.form != nil }

func (v *diagView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	h := "b make bundle · c upload consent · m safe mode · ↑/↓ select incident"
	if v.latestBundle() != nil {
		h = "s send latest bundle · " + h
	}
	return h
}

func (v *diagView) incident() string {
	if v.cursor < 0 || v.cursor >= len(v.data.st.RecentIncidents) {
		return ""
	}
	return v.data.st.RecentIncidents[v.cursor]
}

func (v *diagView) latestBundle() *diag.BundleInfo {
	if len(v.data.st.Bundles) == 0 {
		return nil
	}
	return &v.data.st.Bundles[0]
}

func (v *diagView) bundleForm() tea.Cmd {
	v.mode, v.note, v.source = "bundle", "", "new"
	opts := []huh.Option[string]{huh.NewOption("A new snapshot of the logs and metrics now", "new")}
	if inc := v.incident(); inc != "" {
		opts = append(opts, huh.NewOption("The selected incident: "+inc, "selected"))
		v.source = "selected"
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Make a bundle from").Options(opts...).Value(&v.source),
		huh.NewText().Title("Note").Description("Optional. What you saw, in your own words. It goes in the bundle.").
			Lines(3).CharLimit(hostd.MaxNote).Value(&v.note),
		huh.NewNote().Title("Redacted").Description(
			"The bundle leaves out secrets, addresses, the hostname and serial numbers.\n"+
				"It stays on this host until you send it."),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *diagView) submitForm(b diag.BundleInfo) tea.Cmd {
	if v.data.st.Consent == diag.ConsentOff {
		return note(false, "upload consent is off: press c to allow uploads first")
	}
	if v.data.st.Endpoint == "" {
		return note(false, "no upload endpoint is set: an administrator sets it with sudo sibyl-diag consent manual -endpoint URL")
	}
	v.mode, v.confirm = "submit", false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(fmt.Sprintf("Send %s (%d bytes, redacted)?", b.ID, b.Bytes)).
			Description("It goes to " + v.data.st.Endpoint + ".").
			Affirmative("Send").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *diagView) consentForm() tea.Cmd {
	v.mode, v.consent = "consent", v.data.st.Consent
	if v.consent == "" {
		v.consent = diag.ConsentOff
	}
	v.form = huh.NewForm(huh.NewGroup(consentSelect(&v.consent))).WithShowHelp(false)
	return v.form.Init()
}

// consentSelect is the consent question. The setup wizard asks it too.
func consentSelect(value *string) *huh.Select[string] {
	return huh.NewSelect[string]().Title("Can this host send diagnostic bundles?").
		Description("A bundle holds redacted logs, metrics and the hardware list. You can change this later on the Diagnostics screen.").
		Options(
			huh.NewOption("Off: bundles stay on this host", diag.ConsentOff),
			huh.NewOption("Manual: a bundle goes only when you send it", diag.ConsentManual),
			huh.NewOption("Automatic: the watchdog sends a bundle after each incident", diag.ConsentAutomatic),
		).Value(value)
}

func (v *diagView) safeModeForm() tea.Cmd {
	v.mode, v.confirm = "safe-mode", false
	title, desc, yes := "Turn safe mode on?",
		"llama-swap does not start at the next boot, and the watchdog does not act.", "Turn on"
	if v.data.st.SafeMode.On {
		title, desc, yes = "Turn safe mode off?",
			"This also clears the crash counters. Start inference again on the Status screen.", "Turn off"
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(title).Description(desc).Affirmative(yes).Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *diagView) submit() tea.Cmd {
	run := func(what string, fn func(context.Context) (hostd.Result, error)) tea.Cmd {
		return tea.Batch(note(true, "%s…", what),
			call(fn, func(r hostd.Result, err error) tea.Msg { return diagDone{what, r, err} }))
	}
	switch v.mode {
	case "bundle":
		req := hostd.DiagBundleReq{Note: strings.TrimSpace(v.note)}
		what := "make bundle from a new snapshot (this takes up to a minute)"
		if v.source == "selected" && v.incident() != "" {
			req.Incident = v.incident()
			what = "make bundle from " + req.Incident
		}
		if err := req.Validate(); err != nil {
			return note(false, "bundle: %v", err)
		}
		return run(what, func(ctx context.Context) (hostd.Result, error) { return v.c.MakeBundle(ctx, req) })
	case "submit":
		b := v.latestBundle()
		if !v.confirm || b == nil {
			return note(false, "bundle kept on this host")
		}
		id := b.ID
		return run("send "+id, func(ctx context.Context) (hostd.Result, error) { return v.c.SubmitBundle(ctx, id) })
	case "consent":
		mode := v.consent
		return run("set upload consent to "+mode, func(ctx context.Context) (hostd.Result, error) { return v.c.SetConsent(ctx, mode) })
	case "safe-mode":
		if !v.confirm {
			return note(false, "safe mode not changed")
		}
		on := !v.data.st.SafeMode.On
		return run("set safe mode", func(ctx context.Context) (hostd.Result, error) { return v.c.SetSafeMode(ctx, on) })
	}
	return nil
}

func (v *diagView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case diagTick:
		return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return diagTick{} }))
	case diagLoaded:
		v.data = msg
		if v.cursor >= len(v.data.st.RecentIncidents) {
			v.cursor = 0
		}
		return nil
	case diagDone:
		switch {
		case msg.err != nil:
			return tea.Batch(note(false, "%s: %v", msg.what, msg.err), v.fetch())
		case msg.res.Finished == "":
			return note(true, "%s: queued; the bundle list shows the result when it is done", msg.what)
		}
		return tea.Batch(note(true, "%s", msg.res.Message), v.fetch())
	case tea.KeyPressMsg:
		if v.form == nil {
			return v.key(msg.String())
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

func (v *diagView) key(k string) tea.Cmd {
	switch k {
	case "up":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < len(v.data.st.RecentIncidents)-1 {
			v.cursor++
		}
	case "b":
		return v.bundleForm()
	case "s":
		if b := v.latestBundle(); b != nil {
			return v.submitForm(*b)
		}
	case "c":
		return v.consentForm()
	case "m":
		return v.safeModeForm()
	}
	return nil
}

func (v *diagView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.data.err != nil {
		b.WriteString(badSty.Render("API error: "+v.data.err.Error()) + "\n")
		return b.String()
	}
	st := v.data.st
	b.WriteString(diagSummary(st))

	b.WriteString("\n" + titleSty.Render("Speed per model, last 24 hours") + "\n")
	b.WriteString(perfTable(v.data.perf, v.data.perfErr))

	b.WriteString("\n" + titleSty.Render(fmt.Sprintf("Incidents (%d on this host)", st.Incidents)) + "\n")
	if len(st.RecentIncidents) == 0 {
		b.WriteString(mutedSty.Render("  none · the watchdog records one before it restarts or resets anything") + "\n")
	}
	start, end := window(len(st.RecentIncidents), v.cursor)
	for i := start; i < end; i++ {
		b.WriteString(cursorLine(i == v.cursor, st.RecentIncidents[i]) + "\n")
	}
	b.WriteString(more(start, end, len(st.RecentIncidents)))

	b.WriteString("\n" + titleSty.Render("Bundles") + "\n")
	if len(st.Bundles) == 0 {
		b.WriteString(mutedSty.Render("  none · press b to make one") + "\n")
	}
	for i, bd := range st.Bundles {
		if i == 5 {
			b.WriteString(mutedSty.Render(fmt.Sprintf("  and %d older", len(st.Bundles)-5)) + "\n")
			break
		}
		state := mutedSty.Render(bd.State)
		if bd.State == diag.BundleSent {
			state = goodSty.Render(bd.State)
		}
		fmt.Fprintf(&b, "  %s  %s  %8d bytes  %s\n", bd.ID, bd.Created.Local().Format("2006-01-02 15:04"), bd.Bytes, state)
	}
	if lb := v.latestBundle(); lb != nil {
		b.WriteString("  Latest diagnostic ID: " + selSty.Render(lb.ID) + mutedSty.Render("  give this ID to support") + "\n")
	}
	if !st.Updated.IsZero() {
		b.WriteString(mutedSty.Render("  updated "+st.Updated.Local().Format(time.RFC3339)) + "\n")
	}
	return b.String()
}

// diagSummary renders the watchdog, safe mode and upload lines.
func diagSummary(st diag.StatusFile) string {
	var b strings.Builder
	w := st.Watchdog
	health := goodSty.Render("ok")
	if w.Degraded {
		health = badSty.Render("degraded: the watchdog stopped acting after repeated failures")
	}
	fmt.Fprintf(&b, "Watchdog   %s  %d restarts, %d reboots, %d unclean boots\n", health, w.Restarts, w.Reboots, w.UncleanBoots)
	if w.LastProblem != "" {
		fmt.Fprintf(&b, "           last problem %s, last action %s\n", w.LastProblem, orNone(w.LastAction))
	}
	safe := goodSty.Render("off")
	if st.SafeMode.On {
		safe = badSty.Render("on: llama-swap does not start at boot")
		if st.SafeMode.Reason != "" {
			safe += badSty.Render(" (" + st.SafeMode.Reason + ")")
		}
	}
	fmt.Fprintf(&b, "Safe mode  %s\n", safe)
	fmt.Fprintf(&b, "Upload     consent %s  endpoint %s\n", orNone(st.Consent), orNone(st.Endpoint))
	return b.String()
}

// perfTable renders the speed per model.
func perfTable(p diag.PerfFile, err error) string {
	if err != nil {
		return "  " + mutedSty.Render(err.Error()) + "\n"
	}
	if len(p.Models) == 0 {
		return "  " + mutedSty.Render("no requests in the last 24 hours") + "\n"
	}
	var b strings.Builder
	b.WriteString(mutedSty.Render(fmt.Sprintf("  %-32s %8s %6s %12s %12s %14s", "MODEL", "REQUESTS", "ERRORS", "GEN TOK/S", "SLOW 5%", "PROMPT TOK/S")) + "\n")
	for _, m := range p.Models {
		errs := fmt.Sprintf("%6d", m.Errors)
		if m.Errors > 0 {
			errs = badSty.Render(errs)
		}
		fmt.Fprintf(&b, "  %-32s %8d %s %12s %12s %14s\n", truncateName(m.Model, 32), m.Requests, errs,
			diag.FormatTPS(m.GenTPSP50), diag.FormatTPS(m.GenTPSP5), diag.FormatTPS(m.PromptTPSP50))
	}
	return b.String()
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
