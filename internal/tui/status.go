package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hostd"
)

type statusMsg struct {
	status client.Status
	gpus   []gpu.GPU
	owui   client.OpenWebUI
	arcane client.Arcane
	host   hostd.State
	err    error
}

type owuiSaved struct {
	st  client.OpenWebUI
	err error
}

type statusTick struct{}

type statusView struct {
	c      *client.Client
	data   statusMsg
	units  []string
	cursor int
	ticks  bool
	form   *huh.Form
	step   int // 0: choose a method, 1: credentials, 2: an update or reboot confirm
	mode   string
	// confirm and reboot are the values of the update and reboot forms.
	confirm, reboot bool
	key             string
	name            string
	email           string
	pass            string
}

func newStatusView(c *client.Client) *statusView { return &statusView{c: c} }

func (v *statusView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s, err := v.c.Status(ctx)
		if err != nil {
			return statusMsg{err: err}
		}
		g, err := v.c.GPUs(ctx)
		if err != nil {
			return statusMsg{status: s, err: err}
		}
		// The addresses are extra: a missing host state does not hide the rest.
		h, _ := v.c.Host(ctx)
		a, _ := v.c.Arcane(ctx)
		o, err := v.c.OpenWebUI(ctx)
		return statusMsg{status: s, gpus: g, owui: o, arcane: a, host: h, err: err}
	}
}

func (v *statusView) activate() tea.Cmd {
	if v.ticks {
		return v.fetch()
	}
	v.ticks = true
	return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return statusTick{} }))
}

func (v *statusView) capturing() bool { return v.form != nil }

func (v *statusView) help() string {
	if v.form != nil {
		return "enter next · esc discard"
	}
	return "↑/↓ select · r restart service · o Open WebUI key · u check for an update · U apply it · B reboot into a staged update"
}

func (v *statusView) keyForm() tea.Cmd {
	v.step, v.mode, v.key, v.name, v.email, v.pass = 0, "signin", "", "", "", ""
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Connect Open WebUI").Description(
			"Sibyl sets Open WebUI's document embedding model for you. It needs an\n"+
				"admin API key. It can get one with your admin login; the password is\n"+
				"used once and never stored."),
		huh.NewSelect[string]().Title("How").Options(
			huh.NewOption("Sign in with an existing admin account", "signin"),
			huh.NewOption("Create the first admin account (fresh install)", "create"),
			huh.NewOption("Paste an API key", "key"),
		).Value(&v.mode),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *statusView) credentialsForm() tea.Cmd {
	v.step = 1
	var fields []huh.Field
	switch v.mode {
	case "key":
		fields = append(fields, huh.NewInput().Title("Open WebUI API key").
			Description("Admin Panel, Settings, General: API Keys must be on.").
			EchoMode(huh.EchoModePassword).Value(&v.key))
	default:
		if v.mode == "create" {
			fields = append(fields, huh.NewInput().Title("Name").Placeholder("Admin").Value(&v.name))
		}
		fields = append(fields,
			huh.NewInput().Title("Admin email").Value(&v.email),
			huh.NewInput().Title("Password").EchoMode(huh.EchoModePassword).Value(&v.pass))
	}
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func (v *statusView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case statusTick:
		return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return statusTick{} }))
	case statusMsg:
		v.data = msg
		v.units = v.units[:0]
		for u := range msg.status.Services.Units {
			v.units = append(v.units, u)
		}
		sort.Strings(v.units)
		if v.cursor >= len(v.units) {
			v.cursor = 0
		}
	case owuiSaved:
		if msg.err != nil {
			return note(false, "open webui: %v", msg.err)
		}
		v.data.owui = msg.st
		return note(true, "Open WebUI connected: %s", msg.st.LastSync)
	case tea.KeyPressMsg:
		if v.form != nil {
			if msg.String() == "esc" {
				v.form = nil
				return nil
			}
			return v.formStep(msg)
		}
		switch msg.String() {
		case "o":
			return v.keyForm()
		case "u":
			return v.hostCall("update check", func(ctx context.Context) (hostd.Result, error) { return v.c.CheckUpdate(ctx) })
		case "U":
			if u := v.data.host.Update; u != nil && u.Available != "" {
				return v.updateForm(u.Available)
			}
			return note(false, "no update is known: press u to check")
		case "B":
			if u := v.data.host.Update; u != nil && u.Staged != "" {
				return v.rebootForm(u.Staged)
			}
			return note(false, "no update is staged")
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.units)-1 {
				v.cursor++
			}
		case "r":
			if len(v.units) == 0 {
				return nil
			}
			unit := v.units[v.cursor]
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := v.c.Restart(ctx, unit); err != nil {
					return noteMsg{"restart " + unit + ": " + err.Error(), false}
				}
				return noteMsg{"restart of " + unit + " queued", true}
			}
		}
	}
	if v.form != nil {
		return v.formStep(msg)
	}
	return nil
}

// hostCall runs a host action and reports its result.
func (v *statusView) hostCall(what string, fn func(context.Context) (hostd.Result, error)) tea.Cmd {
	return tea.Batch(note(true, "%s…", what), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := fn(ctx)
		switch {
		case err != nil:
			return noteMsg{what + ": " + err.Error(), false}
		case !res.OK && res.Error != "":
			return noteMsg{what + ": " + res.Error, false}
		}
		return noteMsg{res.Message, true}
	})
}

func (v *statusView) updateForm(version string) tea.Cmd {
	v.step, v.mode, v.confirm, v.reboot = 2, "update", false, true
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Reboot into it when it is staged").
			Description("A reboot stops the models and the APIs for a few minutes. No: press B to reboot later.").
			Affirmative("Reboot").Negative("Later").Value(&v.reboot),
		huh.NewConfirm().Title("Apply update "+version+"?").
			Description("The download runs in the background. If the new version does not boot, the host starts the old one.").
			Affirmative("Apply").Negative("Not now").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *statusView) rebootForm(version string) tea.Cmd {
	v.step, v.mode, v.confirm = 2, "reboot", false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Reboot into " + version + " now?").
			Description("The models and the APIs stop for a few minutes.").
			Affirmative("Reboot").Negative("Not now").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *statusView) formStep(msg tea.Msg) tea.Cmd {
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	if v.form.State != huh.StateCompleted {
		return cmd
	}
	if v.step == 2 {
		v.form = nil
		if !v.confirm {
			return tea.Batch(cmd, note(false, "nothing changed"))
		}
		if v.mode == "reboot" {
			return tea.Batch(cmd, v.hostCall("reboot", func(ctx context.Context) (hostd.Result, error) { return v.c.Reboot(ctx) }))
		}
		reboot := v.reboot
		return tea.Batch(cmd, v.hostCall("update", func(ctx context.Context) (hostd.Result, error) { return v.c.ApplyUpdate(ctx, reboot) }))
	}
	if v.step == 0 {
		return tea.Batch(cmd, v.credentialsForm())
	}
	v.form = nil
	mode, key, name, email, pass := v.mode, strings.TrimSpace(v.key), v.name, strings.TrimSpace(v.email), v.pass
	v.key, v.pass = "", ""
	save := func(ctx context.Context) (client.OpenWebUI, error) {
		if mode == "key" {
			return v.c.SetOpenWebUIKey(ctx, key)
		}
		return v.c.ConnectOpenWebUI(ctx, mode == "create", name, email, pass)
	}
	if (mode == "key" && key == "") || (mode != "key" && (email == "" || pass == "")) {
		return tea.Batch(cmd, note(false, "nothing entered; Open WebUI not connected"))
	}
	return tea.Batch(cmd, note(true, "connecting to Open WebUI…"),
		call(save, func(o client.OpenWebUI, err error) tea.Msg { return owuiSaved{o, err} }))
}

func (v *statusView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.data.err != nil {
		b.WriteString(badSty.Render("API error: "+v.data.err.Error()) + "\n\n")
	}
	s := v.data.status
	fmt.Fprintf(&b, "Version %s · backend %s · layout %s · GPUs in use %v\n\n",
		s.Version, s.Hardware.Backend, s.Hardware.Layout, s.Hardware.GPUs)

	b.WriteString(titleSty.Render("Addresses") + "\n")
	b.WriteString(renderLinks(v.data.host) + "\n")
	if a := v.data.arcane; a.Password != "" {
		b.WriteString("  " + badSty.Render("Arcane login: "+a.User+" / "+a.Password+
			". Sibyl set this password; change it in Arcane, and this line goes away.") + "\n\n")
	}
	b.WriteString(titleSty.Render("GPUs") + "\n")
	for _, g := range v.data.gpus {
		used := ""
		if g.VRAMTotal > 0 {
			used = fmt.Sprintf("%s / %s", gib(int64(g.VRAMUsed)), gib(int64(g.VRAMTotal)))
		}
		fmt.Fprintf(&b, "  %d  %-14s %-7s %-20s PCIe x%s\n", g.Index, g.PCIAddr, g.Vendor, used, g.LinkWidth)
	}
	b.WriteString("\n" + titleSty.Render("Services") + "\n")
	for i, u := range v.units {
		st := s.Services.Units[u]
		state := st.Active + "/" + st.Sub
		if st.Active == "active" {
			state = goodSty.Render(state)
		} else {
			state = badSty.Render(state)
		}
		b.WriteString(cursorLine(i == v.cursor, fmt.Sprintf("%-12s %s", u, state)) + "\n")
	}
	if s.Services.Updated != "" {
		b.WriteString(mutedSty.Render("  updated "+s.Services.Updated) + "\n")
	}
	if u := v.data.host.Update; u != nil {
		b.WriteString("\n" + titleSty.Render("Update") + "\n")
		fmt.Fprintf(&b, "  running %s\n", u.Booted)
		if u.Available != "" {
			b.WriteString("  " + goodSty.Render("available "+u.Available) + " · press U to apply\n")
		}
		if u.Staged != "" {
			b.WriteString("  staged " + u.Staged + " · it starts at the next boot · press B to reboot now\n")
		}
		for _, r := range []hostd.UnitRun{u.Check, u.Download, u.DownloadReboot} {
			if r.Active == "activating" {
				b.WriteString("  " + mutedSty.Render("an update step is running") + "\n")
				break
			}
		}
		if u.Available == "" && u.Staged == "" {
			b.WriteString("  " + mutedSty.Render("press u to check for an update") + "\n")
		}
	}
	b.WriteString("\n" + titleSty.Render("Open WebUI") + "\n")
	o := v.data.owui
	switch {
	case !o.Configured:
		b.WriteString("  " + mutedSty.Render("not connected · press o to add an API key, so Sibyl can set the embedding model") + "\n")
	case o.Error != "":
		b.WriteString("  " + badSty.Render(o.Error) + "\n")
	default:
		b.WriteString("  " + goodSty.Render("connected") + " · " + o.LastSync + "\n")
	}
	return b.String()
}
