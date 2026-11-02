package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/diag"
)

func TestDiagViewRender(t *testing.T) {
	tps := 62.3
	v := newDiagView(nil)
	v.update(diagLoaded{
		st: diag.StatusFile{
			Updated: time.Now(), Consent: diag.ConsentManual, Endpoint: "https://support.example.com",
			Watchdog:        diag.WatchdogStatus{Restarts: 4, LastProblem: "model-unresponsive", LastAction: "gpu-reset"},
			Incidents:       2,
			RecentIncidents: []string{"20261031T114141Z-gpu-fault", "20261031T103035Z-generation-stalled"},
			Bundles: []diag.BundleInfo{
				{ID: "SBL-5V4D-7P0P", Bytes: 727942, State: diag.BundleSent},
				{ID: "SBL-WVBN-18HN", Bytes: 336078, State: diag.BundleLocal},
			},
		},
		perf: diag.PerfFile{Models: []diag.ModelPerf{{Model: "flint-q8_0", Requests: 3, Errors: 1, GenTPSP50: &tps}}},
	})
	got := v.render(120)
	for _, want := range []string{
		"4 restarts", "last problem model-unresponsive, last action gpu-reset", "consent manual",
		"flint-q8_0", "62.3", "Incidents (2 on this host)", "20261031T114141Z-gpu-fault",
		"Latest diagnostic ID: ", "SBL-5V4D-7P0P",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(v.help(), "s send latest bundle") {
		t.Errorf("help %q", v.help())
	}

	v.key("down")
	if v.incident() != "20261031T103035Z-generation-stalled" {
		t.Fatalf("selected %q", v.incident())
	}
	if v.bundleForm(); v.source != "selected" || v.form == nil {
		t.Fatalf("bundle form source %q", v.source)
	}
	v.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.form != nil {
		t.Fatal("esc kept the form")
	}
}

func TestDiagViewRefusesToSendWithoutConsent(t *testing.T) {
	v := newDiagView(nil)
	v.update(diagLoaded{st: diag.StatusFile{Consent: diag.ConsentOff, Bundles: []diag.BundleInfo{{ID: "SBL-5V4D-7P0P"}}}})
	msg := v.key("s")()
	if n, ok := msg.(noteMsg); !ok || n.ok || !strings.Contains(n.text, "consent is off") || v.form != nil {
		t.Fatalf("s with consent off gave %+v", msg)
	}
	v.update(diagLoaded{st: diag.StatusFile{Consent: diag.ConsentManual, Bundles: []diag.BundleInfo{{ID: "SBL-5V4D-7P0P"}}}})
	if n, ok := v.key("s")().(noteMsg); !ok || !strings.Contains(n.text, "no upload endpoint") {
		t.Fatalf("s with no endpoint gave %+v", n)
	}
}

func TestDiagViewShowsErrors(t *testing.T) {
	v := newDiagView(nil)
	v.update(diagLoaded{err: errors.New("503: sibyl-diag has not written status.json yet")})
	if got := v.render(80); !strings.Contains(got, "has not written status.json") {
		t.Fatalf("render %q", got)
	}
	if got := perfTable(diag.PerfFile{}, nil); !strings.Contains(got, "no requests") {
		t.Fatalf("empty perf %q", got)
	}
}
