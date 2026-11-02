package hostd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SetUpdateWindow sets the nightly update window, or turns nightly updates
// off.
const SetUpdateWindow Action = "update-window"

// AutoUpdateTimer runs /usr/libexec/sibyl/auto-update in the window.
const AutoUpdateTimer = "sibyl-auto-update.timer"

// The window of the image's timer unit, in host time.
const (
	DefaultWindowStart = "03:00"
	DefaultWindowEnd   = "05:30"
)

// windowDropIn is the timer drop-in that holds the owner's window, under
// Etc. Its first line records the window, so the state can read it back.
const windowDropIn = "systemd/system/" + AutoUpdateTimer + ".d/window.conf"

// UpdateWindow is the body of an update-window request, and the window in
// the state. The times are host time, HH:MM on the hour or the half hour.
// The timer runs every 30 minutes from Start to End, both included. A
// window can cross midnight, such as 23:00 to 01:00.
type UpdateWindow struct {
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"`
	End     string `json:"end"`
}

var windowTimeRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):(00|30)$`)

// Validate checks the times.
func (w UpdateWindow) Validate() error {
	for name, t := range map[string]string{"start": w.Start, "end": w.End} {
		if !windowTimeRE.MatchString(t) {
			return fmt.Errorf("%s: give a time such as 03:00 or 03:30", name)
		}
	}
	return nil
}

func halfHours(t string) int {
	h := int(t[0]-'0')*10 + int(t[1]-'0')
	return h*2 + int(t[3]-'0')/3
}

// Slots returns the times of the runs, from Start to End.
func (w UpdateWindow) Slots() []string {
	start, end := halfHours(w.Start), halfHours(w.End)
	var out []string
	for i := start; ; i = (i + 1) % 48 {
		out = append(out, fmt.Sprintf("%02d:%02d", i/2, i%2*30))
		if i == end {
			return out
		}
	}
}

// dropIn is the timer drop-in for the window. The empty OnCalendar= drops
// the image's own schedule.
func (w UpdateWindow) dropIn() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# sibyl update window %s-%s. Set it with the Status screen or the web console.\n", w.Start, w.End)
	b.WriteString("[Timer]\nOnCalendar=\n")
	for _, s := range w.Slots() {
		fmt.Fprintf(&b, "OnCalendar=*-*-* %s:00\n", s)
	}
	return b.String()
}

var dropInHeadRE = regexp.MustCompile(`^# sibyl update window ([0-9:]{5})-([0-9:]{5})\.`)

// setUpdateWindow writes the window and turns the timer on or off.
func (a *Agent) setUpdateWindow(ctx context.Context, w UpdateWindow) (string, error) {
	p := filepath.Join(a.Etc, windowDropIn)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(w.dropIn()), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	if _, err := a.Run(ctx, "", "systemctl", "daemon-reload"); err != nil {
		return "", err
	}
	verb, msg := "disable", "nightly updates off"
	if w.Enabled {
		verb, msg = "enable", fmt.Sprintf("nightly updates from %s to %s, host time", w.Start, w.End)
	}
	if _, err := a.Run(ctx, "", "systemctl", verb, "--now", "--", AutoUpdateTimer); err != nil {
		return "", err
	}
	return msg, nil
}

// updateWindow reads the window from the drop-in, or the image's default,
// and whether the timer is on.
func (a *Agent) updateWindow(ctx context.Context) (*UpdateWindow, error) {
	w := &UpdateWindow{Start: DefaultWindowStart, End: DefaultWindowEnd}
	b, err := os.ReadFile(filepath.Join(a.Etc, windowDropIn))
	switch {
	case err == nil:
		if m := dropInHeadRE.FindSubmatch(b); m != nil {
			w.Start, w.End = string(m[1]), string(m[2])
		}
	case !errors.Is(err, os.ErrNotExist):
		return w, err
	}
	// is-enabled exits 1 for a disabled unit, and prints "disabled".
	out, _ := a.Run(ctx, "", "systemctl", "is-enabled", "--", AutoUpdateTimer)
	w.Enabled = strings.TrimSpace(out) == "enabled"
	return w, nil
}
