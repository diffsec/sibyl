// Package diag records inference metrics, watches inference, captures
// incidents, and builds redacted diagnostic bundles. It runs on the host as
// root, as sibyl-diag (cmd/sibyl-diag). The design is in
// docs/diagnostics-design.md.
package diag

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Exec is the production Runner: a fixed PATH and the C locale, so the
// output that sibyl-diag parses does not change with the environment.
func Exec(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin", "LC_ALL=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			err = errors.New(msg)
		}
		return stdout.String(), fmt.Errorf("%s: %w", name, err)
	}
	return stdout.String(), nil
}

// Diag holds the paths, configuration and dependencies of every part.
type Diag struct {
	// Root is "/" on the host. Tests point it at a temporary tree.
	Root string
	Cfg  Config
	Run  Runner
	Now  func() time.Time
	Log  *slog.Logger
	// Label is the SELinux context for the files in Out, so the System API
	// container can read them. Empty skips labelling.
	Label string
	// Group owns the files in Out. -1 keeps the group of the process.
	Group int
}

// Paths relative to Root.
const (
	dataDir         = "var/lib/sibyl-diag"
	safeModeFile    = "var/lib/sibyl/config/safe-mode"
	defaultsFile    = "usr/share/sibyl/diagnostics.yaml"
	overrideFile    = "var/lib/sibyl/config/diagnostics.yaml"
	llamaSwapConfig = "var/lib/sibyl/config/llama-swap/config.yaml"
	pstoreDir       = "var/lib/systemd/pstore"
)

func (d *Diag) path(rel ...string) string {
	return filepath.Join(append([]string{d.Root}, rel...)...)
}

func (d *Diag) data(rel ...string) string { return d.path(append([]string{dataDir}, rel...)...) }

// IncidentDir holds one directory per incident.
func (d *Diag) IncidentDir() string { return d.data("incidents") }

// BundleDir holds the bundles and their upload markers.
func (d *Diag) BundleDir() string { return d.data("bundles") }

// MetricsDir holds the JSON lines files of samples and requests.
func (d *Diag) MetricsDir() string { return d.data("metrics") }

// OutDir holds status.json and perf.json for the System API.
func (d *Diag) OutDir() string { return d.data("out") }

// SafeModeFile is in the config directory, not in the data directory: the
// sibyl user manager evaluates llama-swap's ConditionPathExists on it, and it
// cannot search the root-only data directory.
func (d *Diag) SafeModeFile() string { return d.path(safeModeFile) }

// Config is diagnostics.yaml. The image default is
// /usr/share/sibyl/diagnostics.yaml, and /var/lib/sibyl/config/diagnostics.yaml
// overrides it key by key. Upload consent is not part of it.
type Config struct {
	Capture  CaptureConfig  `yaml:"capture" json:"capture"`
	Watchdog WatchdogConfig `yaml:"watchdog" json:"watchdog"`
	Upload   UploadConfig   `yaml:"upload" json:"upload"`
}

// CaptureConfig sets what the box keeps.
type CaptureConfig struct {
	SampleInterval   int   `yaml:"sample_interval" json:"sample_interval"`
	RetentionDays    int   `yaml:"retention_days" json:"retention_days"`
	MaxIncidents     int   `yaml:"max_incidents" json:"max_incidents"`
	MaxIncidentBytes int64 `yaml:"max_incident_bytes" json:"max_incident_bytes"`
}

// WatchdogConfig sets the probes and the recovery ladder. Times are seconds.
type WatchdogConfig struct {
	Enabled              bool   `yaml:"enabled" json:"enabled"`
	LlamaSwapURL         string `yaml:"llama_swap_url" json:"llama_swap_url"`
	ProbeInterval        int    `yaml:"probe_interval" json:"probe_interval"`
	ProbeTimeout         int    `yaml:"probe_timeout" json:"probe_timeout"`
	FailuresBeforeAction int    `yaml:"failures_before_action" json:"failures_before_action"`
	StallSeconds         int    `yaml:"stall_seconds" json:"stall_seconds"`
	MaxRestarts          int    `yaml:"max_restarts" json:"max_restarts"`
	RestartWindow        int    `yaml:"restart_window" json:"restart_window"`
	GPUReset             bool   `yaml:"gpu_reset" json:"gpu_reset"`
	Reboot               bool   `yaml:"reboot" json:"reboot"`
	MaxReboots           int    `yaml:"max_reboots" json:"max_reboots"`
	RebootWindow         int    `yaml:"reboot_window" json:"reboot_window"`
	IncidentCooldown     int    `yaml:"incident_cooldown" json:"incident_cooldown"`
}

// UploadConfig holds the image default for the ingest endpoint.
type UploadConfig struct {
	Endpoint       string `yaml:"endpoint" json:"endpoint"`
	MaxBundleBytes int64  `yaml:"max_bundle_bytes" json:"max_bundle_bytes"`
}

// DefaultConfig matches os/rootfs/usr/share/sibyl/diagnostics.yaml.
func DefaultConfig() Config {
	return Config{
		Capture: CaptureConfig{SampleInterval: 10, RetentionDays: 7, MaxIncidents: 50, MaxIncidentBytes: 2 << 30},
		Watchdog: WatchdogConfig{
			Enabled: true, LlamaSwapURL: "http://127.0.0.1:18080", ProbeInterval: 30, ProbeTimeout: 10,
			FailuresBeforeAction: 3, StallSeconds: 300, MaxRestarts: 3, RestartWindow: 900,
			GPUReset: true, Reboot: true, MaxReboots: 2, RebootWindow: 21600, IncidentCooldown: 600,
		},
		Upload: UploadConfig{MaxBundleBytes: 50 << 20},
	}
}

// LoadConfig reads the defaults and then the override over them. A missing
// file is skipped. A file that does not parse is an error, so a typo in the
// override does not silently turn the watchdog off.
func (d *Diag) LoadConfig() error {
	cfg := DefaultConfig()
	for _, p := range []string{d.path(defaultsFile), d.path(overrideFile)} {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	d.Cfg = cfg
	return nil
}

// Consent modes.
const (
	ConsentOff       = "off"
	ConsentManual    = "manual"
	ConsentAutomatic = "automatic"
)

// Consent is the upload choice. Only `sibyl-diag consent` writes it, to a
// root-only file, so an edit to diagnostics.yaml cannot turn uploads on.
type Consent struct {
	Mode     string    `json:"mode"`
	Endpoint string    `json:"endpoint,omitempty"`
	Changed  time.Time `json:"changed,omitzero"`
}

// LoadConsent returns the saved choice, or "off".
func (d *Diag) LoadConsent() Consent {
	var c Consent
	b, err := os.ReadFile(d.data("consent.json"))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return Consent{Mode: ConsentOff}
	}
	switch c.Mode {
	case ConsentOff, ConsentManual, ConsentAutomatic:
	default:
		c.Mode = ConsentOff
	}
	return c
}

// SaveConsent sets the mode, and the endpoint when it is not empty.
func (d *Diag) SaveConsent(mode, endpoint string) (Consent, error) {
	switch mode {
	case ConsentOff, ConsentManual, ConsentAutomatic:
	default:
		return Consent{}, fmt.Errorf("mode must be %s, %s or %s", ConsentOff, ConsentManual, ConsentAutomatic)
	}
	c := d.LoadConsent()
	c.Mode, c.Changed = mode, d.Now().UTC()
	if endpoint != "" {
		if err := CheckEndpoint(endpoint); err != nil {
			return Consent{}, err
		}
		c.Endpoint = strings.TrimRight(endpoint, "/")
	}
	return c, d.writeJSON(d.data("consent.json"), c, 0o600)
}

// Endpoint is the consent endpoint, else the image default.
func (d *Diag) Endpoint() string {
	if e := d.LoadConsent().Endpoint; e != "" {
		return e
	}
	return d.Cfg.Upload.Endpoint
}

// DeviceID is a random ID for this box, made on first use. It does not come
// from the machine ID or a serial number.
func (d *Diag) DeviceID() string { return d.randomFile("device-id", 16, true) }

func (d *Diag) redactionSalt() []byte { return []byte(d.randomFile("redaction-salt", 32, false)) }

func (d *Diag) randomFile(name string, n int, uuid bool) string {
	p := d.data(name)
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b))
	}
	v := randomHex(n)
	if uuid {
		v = v[:8] + "-" + v[8:12] + "-4" + v[13:16] + "-a" + v[17:20] + "-" + v[20:32]
	}
	if err := writeAtomic(p, []byte(v+"\n"), 0o600, "", -1); err != nil {
		d.Log.Warn("save "+name, "err", err)
	}
	return v
}

// Version facts from the image.
func (d *Diag) readShare(name string) string {
	b, err := os.ReadFile(d.path("usr/share/sibyl", name))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// SafeMode reports whether the safe-mode flag is set, and its reason.
func (d *Diag) SafeMode() (bool, string) {
	b, err := os.ReadFile(d.SafeModeFile())
	if err != nil {
		return false, ""
	}
	return true, strings.TrimSpace(string(b))
}

// SetSafeMode sets or clears the flag. Clearing it also clears the crash
// counters, so the next crash does not set it again at once.
func (d *Diag) SetSafeMode(on bool, reason string) error {
	if on {
		return writeAtomic(d.SafeModeFile(), []byte(reason+"\n"), 0o644, "", -1)
	}
	if err := os.Remove(d.SafeModeFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	st := d.LoadState()
	st.UncleanBoots, st.Reboots, st.Degraded = nil, nil, false
	return d.SaveState(st)
}

func (d *Diag) writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'), mode, "", -1)
}

// writeOut writes a file that the System API reads.
func (d *Diag) writeOut(name string, v any) error {
	dir := d.OutDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if d.Group >= 0 {
		if err := os.Lchown(dir, -1, d.Group); err != nil {
			return err
		}
	}
	if d.Label != "" {
		if err := setLabel(dir, d.Label); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, name), append(b, '\n'), 0o640, d.Label, d.Group)
}

// writeAtomic writes a file through a temporary file and a rename.
func writeAtomic(path string, data []byte, mode os.FileMode, label string, gid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if gid >= 0 {
		if err := tmp.Chown(-1, gid); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if label != "" {
		if err := setLabel(tmp.Name(), label); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
