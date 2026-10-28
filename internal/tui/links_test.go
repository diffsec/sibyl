package tui

import (
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/hostd"
)

func TestServiceLinks(t *testing.T) {
	st := hostd.State{Interfaces: []hostd.Interface{
		{Name: "enp8s0", State: "connected", Addresses: []string{"192.168.1.26/24"}},
		{Name: "wlp4s0", State: "unmanaged"},
	}}
	got := serviceLinks(st)
	if len(got) != 4 || got[0].url != "https://192.168.1.26/" || got[1].url != "https://192.168.1.26:9443/" ||
		got[2].url != "https://192.168.1.26:8443/v1" {
		t.Fatalf("links %+v", got)
	}
	out := renderLinks(st)
	// OSC 8: ESC ] 8 ; ; URL, then the text, then the closing sequence.
	if !strings.Contains(out, "\x1b]8;;https://192.168.1.26/") {
		t.Fatalf("no hyperlink in %q", out)
	}
	if !strings.Contains(renderLinks(hostd.State{}), "no network address") {
		t.Fatal("no message without an address")
	}
}
