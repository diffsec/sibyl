package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/hostctl"
	"github.com/diffsec/sibyl/internal/hostd"
)

func TestStatusColumns(t *testing.T) {
	v := newStatusView(nil)
	units := map[string]hostctl.UnitState{}
	for _, u := range hostctl.Units {
		units[u] = hostctl.UnitState{Active: "active", Sub: "running"}
	}
	v.update(statusMsg{
		status: client.Status{Version: "1", Services: hostctl.Status{Units: units}},
		host: hostd.State{Interfaces: []hostd.Interface{{Name: "eth0", State: "connected", Addresses: []string{"192.168.1.26/24"}}},
			Update: &hostd.UpdateState{Booted: "sha-1"}},
		running: &client.Running{},
	})
	for _, width := range []int{80, 140} {
		out := v.render(width)
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			if w := ansi.StringWidth(l); width >= statusWideWidth && w > width {
				t.Errorf("width %d: line %d is %d wide", width, i, w)
			}
		}
		plain := ansi.Strip(out)
		for _, want := range []string{"Services", "GPUs", "Loaded models", "Addresses", "https://192.168.1.26/", "Update", "Open WebUI"} {
			if !strings.Contains(plain, want) {
				t.Errorf("width %d: no %q:\n%s", width, want, plain)
			}
		}
		// A click on the line of the third service selects it.
		row := -1
		for i, l := range lines {
			if strings.Contains(ansi.Strip(l), v.units[2]+" ") {
				row = i
				break
			}
		}
		v.cursor = 0
		v.clickLine(row)
		if v.cursor != 2 {
			t.Errorf("width %d: a click on line %d selected %d", width, row, v.cursor)
		}
	}
	if !strings.Contains(ansi.Strip(v.render(140)), " │ ") {
		t.Error("no columns at 140")
	}
}
