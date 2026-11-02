package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/hostd"
)

// tailscaleText is the Tailscale part of the Host screen.
func tailscaleText(t *hostd.TailscaleState) string {
	var b strings.Builder
	b.WriteString(titleSty.Render("Tailscale") + "\n")
	switch {
	case t == nil:
		b.WriteString("  " + mutedSty.Render("not in this host image") + "\n")
	case t.Backend == "Running":
		b.WriteString("  " + goodSty.Render("on the tailnet "+t.Tailnet) + "\n")
		b.WriteString("  " + t.DNSName + "  " + strings.Join(t.IPs, " ") + "\n")
		b.WriteString("  " + mutedSty.Render("Every port of the host is open to the tailnet. Its access rules decide who gets in.") + "\n")
	case t.AuthURL != "":
		b.WriteString("  " + badSty.Render("waiting for a login. Open this link on any device:") + "\n")
		b.WriteString("  " + link(t.AuthURL) + "\n")
	case t.Enabled:
		state := t.Backend
		if state == "" {
			state = "starting"
		}
		b.WriteString("  " + mutedSty.Render(state) + "\n")
	default:
		b.WriteString("  " + mutedSty.Render("off · press t to join a tailnet") + "\n")
	}
	if t != nil && t.Error != "" {
		b.WriteString("  " + badSty.Render(t.Error) + "\n")
	}
	return b.String()
}

// tailscaleOn tells whether tailscaled runs or waits for a login.
func tailscaleOn(t *hostd.TailscaleState) bool {
	return t != nil && (t.Enabled || t.Backend != "")
}

// tailscaleForm joins a tailnet, or leaves it.
func (v *hostView) tailscaleForm() tea.Cmd {
	t := v.st.Tailscale
	if t == nil {
		return note(false, "this host image has no Tailscale; update the host")
	}
	if t.Backend == "Running" {
		v.mode, v.confirm = "tsdown", false
		v.form = huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Leave the tailnet " + t.Tailnet + "?").
				Description("The host logs out and stops Tailscale. Devices on the tailnet lose their way in.").
				Affirmative("Leave").Negative("Stay").Value(&v.confirm),
		)).WithShowHelp(false)
		return v.form.Init()
	}
	v.mode, v.tsHow, v.tsKey = "tsup", "link", ""
	desc := "The host gets a login link. Open it on any device, and sign in to Tailscale."
	if tailscaleOn(t) {
		desc += "\nChoose Off to stop Tailscale instead."
	}
	how := []huh.Option[string]{
		huh.NewOption("Login link", "link"),
		huh.NewOption("Auth key from the Tailscale admin console", "key"),
	}
	if tailscaleOn(t) {
		how = append(how, huh.NewOption("Off: stop Tailscale", "off"))
	}
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Join a tailnet").Description(desc).Options(how...).Value(&v.tsHow),
		),
		huh.NewGroup(
			huh.NewInput().Title("Auth key").
				Description("It starts with tskey-. The host uses it once and does not keep it.").
				EchoMode(huh.EchoModePassword).Value(&v.tsKey).
				Validate(func(s string) error {
					if !hostd.AuthKeyRE.MatchString(strings.TrimSpace(s)) {
						return errAuthKey
					}
					return nil
				}),
		).WithHideFunc(func() bool { return v.tsHow != "key" }),
	).WithShowHelp(false)
	return v.form.Init()
}

type authKeyError struct{}

func (authKeyError) Error() string { return "paste a key that starts with tskey-" }

var errAuthKey = authKeyError{}

// submitTailscale runs the answer of the Tailscale form.
func (v *hostView) submitTailscale(run func(string, func(context.Context) (hostd.Result, error)) tea.Cmd) tea.Cmd {
	switch {
	case v.mode == "tsdown" && !v.confirm:
		return note(false, "the host stays on the tailnet")
	case v.mode == "tsdown" || v.tsHow == "off":
		return run("leave the tailnet", func(ctx context.Context) (hostd.Result, error) { return v.c.TailscaleDown(ctx) })
	}
	key := ""
	if v.tsHow == "key" {
		key = strings.TrimSpace(v.tsKey)
	}
	v.tsKey = ""
	return run("join the tailnet (this takes up to a minute)", func(ctx context.Context) (hostd.Result, error) {
		return v.c.TailscaleUp(ctx, key)
	})
}
