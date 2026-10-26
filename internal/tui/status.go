package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/gpu"
)

type statusMsg struct {
	status client.Status
	gpus   []gpu.GPU
	err    error
}

type statusTick struct{}

type statusView struct {
	c      *client.Client
	data   statusMsg
	units  []string
	cursor int
	ticks  bool
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
		return statusMsg{status: s, gpus: g, err: err}
	}
}

func (v *statusView) activate() tea.Cmd {
	if v.ticks {
		return v.fetch()
	}
	v.ticks = true
	return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return statusTick{} }))
}

func (v *statusView) capturing() bool { return false }

func (v *statusView) help() string { return "↑/↓ select · r restart service" }

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
	case tea.KeyPressMsg:
		switch msg.String() {
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
	return nil
}

func (v *statusView) render(int) string {
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
	return b.String()
}
