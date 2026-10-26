package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/gpu"
)

type statusMsg struct {
	status client.Status
	gpus   []gpu.GPU
	owui   client.OpenWebUI
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
	key    string
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
		o, err := v.c.OpenWebUI(ctx)
		return statusMsg{status: s, gpus: g, owui: o, err: err}
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
		return "enter save · esc discard"
	}
	return "↑/↓ select · r restart service · o Open WebUI key"
}

func (v *statusView) keyForm() tea.Cmd {
	v.key = ""
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Connect Open WebUI").Description(
			"Sibyl sets Open WebUI's document embedding model for you.\n"+
				"1. In Open WebUI: Admin Panel, Settings, General: turn on API Keys.\n"+
				"2. As an admin: Settings, Account, API Keys: create a key.\n"+
				"3. Paste it here. Sibyl checks it before saving."),
		huh.NewInput().Title("Open WebUI API key").EchoMode(huh.EchoModePassword).Value(&v.key),
	)).WithShowHelp(false)
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

func (v *statusView) formStep(msg tea.Msg) tea.Cmd {
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	if v.form.State != huh.StateCompleted {
		return cmd
	}
	v.form = nil
	key := strings.TrimSpace(v.key)
	v.key = ""
	if key == "" {
		return cmd
	}
	return tea.Batch(cmd, call(func(ctx context.Context) (client.OpenWebUI, error) { return v.c.SetOpenWebUIKey(ctx, key) },
		func(o client.OpenWebUI, err error) tea.Msg { return owuiSaved{o, err} }))
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
