package hostd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateWindowSlots(t *testing.T) {
	for _, c := range []struct {
		start, end string
		want       string
	}{
		{"03:00", "05:30", "03:00 03:30 04:00 04:30 05:00 05:30"},
		{"23:00", "00:30", "23:00 23:30 00:00 00:30"},
		{"02:30", "02:30", "02:30"},
	} {
		w := UpdateWindow{Start: c.start, End: c.end}
		if got := strings.Join(w.Slots(), " "); got != c.want {
			t.Errorf("%s-%s: %s, want %s", c.start, c.end, got, c.want)
		}
	}
	for _, bad := range []string{
		`{"enabled":true,"start":"3:00","end":"05:30"}`,
		`{"enabled":true,"start":"03:15","end":"05:30"}`,
		`{"enabled":true,"start":"24:00","end":"05:30"}`,
		`{"enabled":true,"start":"03:00","end":"05:30","command":"reboot"}`,
	} {
		if _, err := Decode(SetUpdateWindow, []byte(bad)); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
}

func TestSetUpdateWindow(t *testing.T) {
	ta := newAgent(t)
	ta.Etc = t.TempDir()
	if r := ta.submit(t, "1", SetUpdateWindow, `{"enabled":true,"start":"23:30","end":"00:30"}`); !r.OK {
		t.Fatalf("set: %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(ta.Etc, windowDropIn))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Timer]\nOnCalendar=\nOnCalendar=*-*-* 23:30:00\nOnCalendar=*-*-* 00:00:00\nOnCalendar=*-*-* 00:30:00\n"
	if !strings.HasSuffix(string(b), want) {
		t.Errorf("drop-in:\n%s", b)
	}
	for _, c := range []string{"systemctl daemon-reload", "systemctl enable --now -- " + AutoUpdateTimer} {
		if !ta.host.ran(c) {
			t.Errorf("did not run %q: %v", c, ta.host.calls)
		}
	}

	ta.host.answers["systemctl is-enabled -- "+AutoUpdateTimer] = "enabled\n"
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := ta.state(t).Update.Window; w == nil || !w.Enabled || w.Start != "23:30" || w.End != "00:30" {
		t.Errorf("state window %+v", w)
	}

	ta.host.calls = nil
	if r := ta.submit(t, "2", SetUpdateWindow, `{"enabled":false,"start":"23:30","end":"00:30"}`); !r.OK {
		t.Fatalf("off: %+v", r)
	}
	if !ta.host.ran("systemctl disable --now -- " + AutoUpdateTimer) {
		t.Errorf("did not disable the timer: %v", ta.host.calls)
	}
}

func TestUpdateWindowDefault(t *testing.T) {
	ta := newAgent(t)
	ta.Etc = t.TempDir()
	ta.host.answers["systemctl is-enabled -- "+AutoUpdateTimer] = "enabled\n"
	w, err := ta.updateWindow(context.Background())
	if err != nil || !w.Enabled || w.Start != DefaultWindowStart || w.End != DefaultWindowEnd {
		t.Errorf("default window %+v %v", w, err)
	}
}

// TestImageTimerMatchesDefault keeps the image's timer in step with the
// default window that the state reports when there is no drop-in.
func TestImageTimerMatchesDefault(t *testing.T) {
	b, err := os.ReadFile("../../os/rootfs/usr/lib/systemd/system/" + AutoUpdateTimer)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "OnCalendar=") {
			got = append(got, l)
		}
	}
	var want []string
	for _, s := range (UpdateWindow{Start: DefaultWindowStart, End: DefaultWindowEnd}).Slots() {
		want = append(want, "OnCalendar=*-*-* "+s+":00")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the timer runs at\n%s\nbut the default window is\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
