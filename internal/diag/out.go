package diag

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// listLimit is how many incidents and bundles status.json lists.
const listLimit = 20

// StatusFile is what `sibyl-diag status` prints and out/status.json holds.
// The System API serves it as GET /diagnostics.
type StatusFile struct {
	Updated  time.Time `json:"updated"`
	Version  string    `json:"version"`
	DeviceID string    `json:"device_id"`
	Consent  string    `json:"consent"`
	// ConsentChanged is when a person last chose the consent mode. It is
	// zero until the first choice, so "off" by default is not a choice.
	ConsentChanged time.Time      `json:"consent_changed,omitzero"`
	Endpoint       string         `json:"endpoint"`
	SafeMode       SafeModeStatus `json:"safe_mode"`
	Watchdog       WatchdogStatus `json:"watchdog"`
	// Incidents is the number of incidents on the box.
	Incidents      int    `json:"incidents"`
	LatestIncident string `json:"latest_incident"`
	// RecentIncidents are the newest incident names, newest first.
	RecentIncidents []string `json:"recent_incidents"`
	// Bundles are the newest bundles, newest first.
	Bundles []BundleInfo `json:"bundles"`
}

// SafeModeStatus is the safe-mode flag and its reason.
type SafeModeStatus struct {
	On     bool   `json:"on"`
	Reason string `json:"reason"`
}

// WatchdogStatus is a summary of the recovery history.
type WatchdogStatus struct {
	Degraded     bool   `json:"degraded"`
	LastProblem  string `json:"last_problem"`
	LastAction   string `json:"last_action"`
	Restarts     int    `json:"restarts"`
	Reboots      int    `json:"reboots"`
	UncleanBoots int    `json:"unclean_boots"`
}

// Bundle upload states.
const (
	BundleLocal   = "local"   // made, not sent
	BundlePending = "pending" // queued for the upload timer
	BundleSent    = "sent"
)

// BundleInfo is one bundle on the box.
type BundleInfo struct {
	ID      string    `json:"diagnostic_id"`
	Created time.Time `json:"created"`
	Bytes   int64     `json:"bytes"`
	State   string    `json:"state"`
}

// PerfFile is out/perf.json. The System API serves it as
// GET /diagnostics/perf.
type PerfFile struct {
	Updated     time.Time   `json:"updated"`
	WindowHours int         `json:"window_hours"`
	Models      []ModelPerf `json:"models"`
}

// Status returns the current summary.
func (d *Diag) Status() StatusFile {
	st := d.LoadState()
	safe, why := d.SafeMode()
	incidents := d.Incidents()
	consent := d.LoadConsent()
	s := StatusFile{
		Updated:        d.Now().UTC(),
		Version:        d.readShare("version"),
		DeviceID:       d.DeviceID(),
		Consent:        consent.Mode,
		ConsentChanged: consent.Changed,
		Endpoint:       d.Endpoint(),
		SafeMode:       SafeModeStatus{On: safe, Reason: why},
		Watchdog: WatchdogStatus{
			Degraded: st.Degraded, LastProblem: st.LastProblem, LastAction: st.LastAction,
			Restarts: len(st.Restarts), Reboots: len(st.Reboots), UncleanBoots: len(st.UncleanBoots),
		},
		Incidents:       len(incidents),
		RecentIncidents: []string{},
		Bundles:         d.Bundles(listLimit),
	}
	for i := len(incidents) - 1; i >= 0 && len(s.RecentIncidents) < listLimit; i-- {
		s.RecentIncidents = append(s.RecentIncidents, incidents[i])
	}
	if len(incidents) > 0 {
		s.LatestIncident = incidents[len(incidents)-1]
	}
	return s
}

// Bundles lists at most n bundles, newest first.
func (d *Diag) Bundles(n int) []BundleInfo {
	paths, _ := filepath.Glob(filepath.Join(d.BundleDir(), "SBL-*.tar.gz"))
	out := []BundleInfo{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		b := BundleInfo{
			ID:      strings.TrimSuffix(filepath.Base(p), ".tar.gz"),
			Created: info.ModTime().UTC(),
			Bytes:   info.Size(),
			State:   BundleLocal,
		}
		if _, err := os.Stat(p + ".sent"); err == nil {
			b.State = BundleSent
		} else if _, err := os.Stat(p + ".pending"); err == nil {
			b.State = BundlePending
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// WriteOut writes status.json and perf.json for the System API.
func (d *Diag) WriteOut() error {
	reqs, err := d.Requests(d.Now().Add(-24*time.Hour), "")
	if err != nil {
		return err
	}
	if err := d.writeOut("perf.json", PerfFile{Updated: d.Now().UTC(), WindowHours: 24, Models: Perf(reqs)}); err != nil {
		return err
	}
	return d.writeOut("status.json", d.Status())
}
