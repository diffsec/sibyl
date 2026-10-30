package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/gpu"
)

// The watchdog probes llama-swap and every loaded llama-server each
// ProbeInterval. After FailuresBeforeAction failed probes in a row, it
// captures an incident and then climbs a recovery ladder:
//
//  1. Restart llama-swap, at most MaxRestarts times per RestartWindow.
//  2. With gpu_reset on, which is off by default, reset each AMD GPU through
//     debugfs, once per RestartWindow, and restart.
//  3. Reboot, at most MaxReboots times per RebootWindow.
//  4. Stop acting and report "degraded", so a broken box stays up for
//     diagnosis instead of rebooting forever.
//
// A kernel lockup stops this process too. The hardware watchdog that systemd
// arms (system.conf.d/60-sibyl-watchdog.conf) resets the board then, and
// BootCheck records the crash at the next boot.

// Ladder steps.
const (
	ActRestart  = "restart"
	ActGPUReset = "gpu-reset"
	ActReboot   = "reboot"
	ActDegraded = "degraded"
)

// Problems that the probes report.
const (
	ProbServiceFailed    = "service-failed"
	ProbCrashLoop        = "crash-loop"
	ProbAPIUnresponsive  = "api-unresponsive"
	ProbModelUnreachable = "model-unresponsive"
	ProbStalled          = "generation-stalled"
	ProbGPUFault         = "gpu-fault"
)

// gpuFault matches kernel lines that mean the GPU or its driver failed.
var gpuFault = regexp.MustCompile(`(?i)amdgpu.*(ring \S+ timeout|GPU reset|VRAM is lost|page fault|GPU hang|` +
	`IB test failed|SMU is not ready|Fatal error)|NVRM: Xid|i915.*GPU HANG|xe .*(reset|hang)|` +
	`BUG: soft lockup|blocked for more than \d+ seconds|Out of memory: Killed process`)

// progressCounters move while llama-server works on a request.
// n_decode_total moves once per batch, so it also covers a long prompt.
var progressCounters = []string{genTokens, promptTokens, "llamacpp:n_decode_total"}

// transitions are unit states in which llama-swap starts or stops. An image
// pull at start can take minutes, so the HTTP probes wait.
var transitions = map[string]bool{"activating": true, "deactivating": true, "reloading": true, "refreshing": true}

// State is the recovery history, kept across restarts and reboots in
// /var/lib/sibyl-diag/watchdog.json. Times are Unix seconds.
type State struct {
	Restarts      []int64          `json:"restarts"`
	GPUResets     []int64          `json:"gpu_resets"`
	Reboots       []int64          `json:"reboots"`
	UncleanBoots  []int64          `json:"unclean_boots"`
	Crashes       []int64          `json:"crashes"`
	LastIncident  map[string]int64 `json:"last_incident"`
	Degraded      bool             `json:"degraded"`
	JournalCursor string           `json:"journal_cursor"`
	NRestarts     int              `json:"nrestarts"`
	LastProblem   string           `json:"last_problem"`
	LastAction    string           `json:"last_action"`
	LastActionAt  int64            `json:"last_action_at"`
	LastGPUFault  int64            `json:"last_gpu_fault"`
}

// LoadState reads the history. A missing or broken file is a fresh state.
// Unclean boots and reboots from before the last "safe-mode off" are
// dropped here, so a process that saves an older copy cannot bring them back.
func (d *Diag) LoadState() State {
	st := State{NRestarts: -1}
	if b, err := os.ReadFile(d.data("watchdog.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if b, err := os.ReadFile(d.data("safe-mode-cleared")); err == nil {
		if cleared, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			after := func(ts []int64) []int64 {
				var out []int64
				for _, t := range ts {
					if t > cleared {
						out = append(out, t)
					}
				}
				return out
			}
			st.UncleanBoots, st.Reboots = after(st.UncleanBoots), after(st.Reboots)
		}
	}
	if st.LastIncident == nil {
		st.LastIncident = map[string]int64{}
	}
	return st
}

// SaveState writes the history.
func (d *Diag) SaveState(st State) error { return d.writeJSON(d.data("watchdog.json"), st, 0o640) }

func recent(ts []int64, now int64, window int) int {
	n := 0
	for _, t := range ts {
		if now-t < int64(window) {
			n++
		}
	}
	return n
}

func keep(ts []int64, now int64, window int) []int64 {
	var out []int64
	for _, t := range ts {
		if now-t < int64(window) {
			out = append(out, t)
		}
	}
	return out
}

// Decide picks the next ladder step. It reads only its arguments, so the
// tests cover every step.
func Decide(st State, cfg WatchdogConfig, now int64, problem string, hasAMD bool) string {
	// systemd already restarts a crash loop, so another restart does not help.
	if problem != ProbCrashLoop && recent(st.Restarts, now, cfg.RestartWindow) < cfg.MaxRestarts {
		return ActRestart
	}
	// A unit that does not stay up, with no GPU fault in the kernel log, has a
	// configuration or image problem. A GPU reset or a reboot does not fix it.
	if (problem == ProbCrashLoop || problem == ProbServiceFailed) && now-st.LastGPUFault >= int64(cfg.RestartWindow) {
		return ActDegraded
	}
	if cfg.GPUReset && hasAMD && recent(st.GPUResets, now, cfg.RestartWindow) == 0 {
		return ActGPUReset
	}
	if cfg.Reboot && recent(st.Reboots, now, cfg.RebootWindow) < cfg.MaxReboots {
		return ActReboot
	}
	return ActDegraded
}

// Problem is one probe failure.
type Problem struct {
	Name    string
	Details map[string]any
}

type modelProbe struct {
	failures     int
	progress     float64
	seen         bool
	stalledSince int64
}

// Watchdog holds the probe state between ticks.
type Watchdog struct {
	d            *Diag
	swap         *LlamaSwap
	gpus         []gpu.GPU
	st           State
	swapFailures int
	models       map[string]*modelProbe
}

// NewWatchdog loads the history and detects the GPUs.
func NewWatchdog(d *Diag) *Watchdog {
	gpus, err := gpu.Detect(d.path("sys"))
	if err != nil {
		d.Log.Warn("detect GPUs", "err", err)
	}
	timeout := time.Duration(d.Cfg.Watchdog.ProbeTimeout) * time.Second
	return &Watchdog{d: d, swap: NewLlamaSwap(d.Cfg.Watchdog.LlamaSwapURL, timeout), gpus: gpus,
		st: d.LoadState(), models: map[string]*modelProbe{}}
}

func (w *Watchdog) userSystemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return w.d.Run(ctx, "setpriv", AsSibyl("systemctl", append([]string{"--user"}, args...)...)...)
}

func (w *Watchdog) unitProperty(ctx context.Context, prop string) string {
	out, err := w.userSystemctl(ctx, "show", "-p", prop, "--value", "llama-swap.service")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// checkUnit counts systemd's own restarts, to find a crash loop.
func (w *Watchdog) checkUnit(ctx context.Context, active string, now int64) *Problem {
	cfg := w.d.Cfg.Watchdog
	if n, err := strconv.Atoi(w.unitProperty(ctx, "NRestarts")); err == nil {
		if w.st.NRestarts >= 0 && n > w.st.NRestarts {
			for range n - w.st.NRestarts {
				w.st.Crashes = append(w.st.Crashes, now)
			}
		}
		w.st.NRestarts = n
	}
	if c := recent(w.st.Crashes, now, cfg.RestartWindow); c >= cfg.MaxRestarts {
		return &Problem{ProbCrashLoop, map[string]any{"crashes_in_window": c, "active": active}}
	}
	if active == "failed" {
		return &Problem{ProbServiceFailed, map[string]any{"active": active}}
	}
	return nil
}

func (w *Watchdog) checkSwap(ctx context.Context) *Problem {
	err := w.swap.Health(ctx)
	if err == nil {
		w.swapFailures = 0
		return nil
	}
	w.swapFailures++
	if w.swapFailures >= w.d.Cfg.Watchdog.FailuresBeforeAction {
		return &Problem{ProbAPIUnresponsive, map[string]any{"error": err.Error(), "failures": w.swapFailures}}
	}
	return nil
}

func (w *Watchdog) checkModels(ctx context.Context, now int64) *Problem {
	cfg := w.d.Cfg.Watchdog
	running, err := w.swap.Running(ctx)
	if err != nil {
		return nil // checkSwap reports this case
	}
	ready := map[string]bool{}
	for _, r := range running {
		if r.State == "ready" {
			ready[r.Model] = true
		}
	}
	for m := range w.models {
		if !ready[m] {
			delete(w.models, m)
		}
	}
	for m := range ready {
		p := w.models[m]
		if p == nil {
			p = &modelProbe{}
			w.models[m] = p
		}
		err := w.swap.UpstreamHealth(ctx, m)
		var metrics Metrics
		if err == nil {
			metrics, err = w.swap.UpstreamMetrics(ctx, m)
		}
		if err != nil {
			p.failures++
			if p.failures >= cfg.FailuresBeforeAction {
				return &Problem{ProbModelUnreachable, map[string]any{"model": m, "error": err.Error()}}
			}
			continue
		}
		p.failures = 0
		var progress float64
		for _, c := range progressCounters {
			progress += metrics[c]
		}
		busy := metrics["llamacpp:requests_processing"] > 0
		if busy && p.seen && progress == p.progress {
			if p.stalledSince == 0 {
				p.stalledSince = now
			}
			if now-p.stalledSince >= int64(cfg.StallSeconds) {
				return &Problem{ProbStalled, map[string]any{"model": m, "stalled_seconds": now - p.stalledSince}}
			}
		} else {
			p.stalledSince = 0
		}
		p.progress, p.seen = progress, true
	}
	return nil
}

// checkKernel returns new kernel lines that match gpuFault.
func (w *Watchdog) checkKernel(ctx context.Context) []string {
	args := []string{"-k", "--no-pager", "-o", "cat", "--show-cursor"}
	if w.st.JournalCursor != "" {
		args = append(args, "--after-cursor", w.st.JournalCursor)
	} else {
		args = append(args, "-n", "0")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := w.d.Run(ctx, "journalctl", args...)
	if err != nil && out == "" {
		return nil
	}
	var hits []string
	for _, line := range strings.Split(out, "\n") {
		if c, ok := strings.CutPrefix(line, "-- cursor: "); ok {
			w.st.JournalCursor = strings.TrimSpace(c)
		} else if gpuFault.MatchString(line) {
			hits = append(hits, line)
		}
	}
	return hits
}

// capture records an incident, at most once per IncidentCooldown per reason.
func (w *Watchdog) capture(ctx context.Context, reason string, details map[string]any) {
	now := w.d.Now().Unix()
	if now-w.st.LastIncident[reason] < int64(w.d.Cfg.Watchdog.IncidentCooldown) {
		return
	}
	w.st.LastIncident[reason] = now
	w.save()
	dir, err := w.d.Capture(ctx, reason, details, false)
	if err != nil {
		w.d.Log.Error("incident capture", "err", err)
		return
	}
	w.d.Log.Info("incident", "dir", dir)
	if w.d.LoadConsent().Mode == ConsentAutomatic {
		if b, err := w.d.BuildBundle(dir, ""); err == nil {
			w.d.MarkPending(b.Path)
		} else {
			w.d.Log.Error("bundle", "err", err)
		}
	}
}

func (w *Watchdog) save() {
	if err := w.d.SaveState(w.st); err != nil {
		w.d.Log.Error("save watchdog state", "err", err)
	}
}

// gpuResetTimeout bounds one GPU reset. Tests shorten it.
var gpuResetTimeout = time.Minute

// settle returns the seconds that the watchdog waits after an action before
// it acts again, so that llama-swap can start and load a model.
func settle(cfg WatchdogConfig) int64 {
	return int64(max(120, 2*cfg.ProbeInterval*cfg.FailuresBeforeAction))
}

// gpuReset reads each AMD GPU's recover file. A read that fails or hangs
// means the driver is stuck. A read of a wedged GPU can stay in the kernel
// for good, and then the process cannot be killed or waited for, so the
// reset gives up after the timeout and leaves that read behind.
func (w *Watchdog) gpuReset(ctx context.Context) error {
	done := 0
	for _, g := range w.gpus {
		if g.Vendor != "amd" {
			continue
		}
		f := gpu.RecoverFile(w.d.path("sys"), g)
		if f == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, gpuResetTimeout)
		res := make(chan error, 1)
		go func() {
			_, err := w.d.Run(cctx, "cat", f)
			res <- err
		}()
		var err error
		select {
		case err = <-res:
		case <-cctx.Done():
			err = fmt.Errorf("the GPU reset did not finish in %s", gpuResetTimeout)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("reset %s: %w", g.PCIAddr, err)
		}
		done++
	}
	if done == 0 {
		return errors.New("no amdgpu recover file in debugfs")
	}
	return nil
}

func (w *Watchdog) act(ctx context.Context, p Problem) {
	cfg := w.d.Cfg.Watchdog
	now := w.d.Now().Unix()
	hasAMD := false
	for _, g := range w.gpus {
		hasAMD = hasAMD || g.Vendor == "amd"
	}
	action := Decide(w.st, cfg, now, p.Name, hasAMD)
	w.st.LastProblem, w.st.LastAction, w.st.LastActionAt = p.Name, action, now
	w.d.Log.Warn("inference problem", "problem", p.Name, "details", p.Details, "action", action)
	details := map[string]any{"action": action}
	for k, v := range p.Details {
		details[k] = v
	}
	w.capture(ctx, p.Name, details)

	if action == ActGPUReset {
		// Save first. If the reset hangs and systemd kills this process,
		// the next start goes on to a reboot instead of a second reset.
		w.st.GPUResets = append(w.st.GPUResets, now)
		w.save()
		if err := w.gpuReset(ctx); err != nil {
			w.d.Log.Error("GPU reset", "err", err)
			action = ActDegraded
			if cfg.Reboot && recent(w.st.Reboots, now, cfg.RebootWindow) < cfg.MaxReboots {
				action = ActReboot
			}
			w.st.LastAction = action
		}
	}
	switch action {
	case ActRestart, ActGPUReset:
		w.st.Restarts = append(w.st.Restarts, now)
		if _, err := w.userSystemctl(ctx, "restart", "--no-block", "llama-swap.service"); err != nil {
			w.d.Log.Error("restart llama-swap", "err", err)
		}
	case ActReboot:
		w.st.Reboots = append(w.st.Reboots, now)
		w.save()
		if _, err := w.d.Run(ctx, "systemctl", "reboot"); err != nil {
			w.d.Log.Error("reboot", "err", err)
		}
	case ActDegraded:
		w.st.Degraded = true
	}
	w.swapFailures = 0
	clear(w.models)
}

// Tick runs every probe once and acts on the first problem.
func (w *Watchdog) Tick(ctx context.Context) {
	cfg := w.d.Cfg.Watchdog
	now := w.d.Now().Unix()
	// Reload, so a change from "sibyl-diag safe-mode off" is not overwritten.
	w.st = w.d.LoadState()
	window := max(cfg.RestartWindow, cfg.RebootWindow)
	w.st.Restarts, w.st.GPUResets = keep(w.st.Restarts, now, window), keep(w.st.GPUResets, now, window)
	w.st.Reboots, w.st.Crashes = keep(w.st.Reboots, now, window), keep(w.st.Crashes, now, window)
	defer w.save()

	if faults := w.checkKernel(ctx); len(faults) > 0 {
		w.st.LastGPUFault = now
		w.capture(ctx, ProbGPUFault, map[string]any{"kernel": faults[max(0, len(faults)-20):]})
	}
	if on, _ := w.d.SafeMode(); on {
		return
	}
	active := w.unitProperty(ctx, "ActiveState")
	p := w.checkUnit(ctx, active, now)
	if p == nil && transitions[active] {
		w.swapFailures = 0
		return
	}
	if p == nil && active == "inactive" {
		return // stopped on purpose, for example from Arcane
	}
	if p == nil {
		p = w.checkSwap(ctx)
	}
	if p == nil {
		p = w.checkModels(ctx, now)
	}
	switch {
	case p != nil && !w.st.Degraded && now-w.st.LastActionAt >= settle(cfg):
		w.act(ctx, *p)
	case p == nil && w.st.Degraded:
		w.d.Log.Info("inference is healthy again")
		w.st.Degraded = false
	}
}

// RunWatchdog probes until ctx ends.
func (d *Diag) RunWatchdog(ctx context.Context) error {
	w := NewWatchdog(d)
	Notify("READY=1")
	interval := time.Duration(max(1, d.Cfg.Watchdog.ProbeInterval)) * time.Second
	for {
		start := time.Now()
		if d.Cfg.Watchdog.Enabled {
			w.Tick(ctx)
		}
		status := "ok"
		if on, _ := d.SafeMode(); on {
			status = "safe mode"
		} else if w.st.Degraded {
			status = "degraded"
		}
		Notify("WATCHDOG=1\nSTATUS=" + status)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(max(time.Second, interval-time.Since(start))):
		}
	}
}

// BootCheck runs once at boot, before the sibyl user manager. It records an
// unclean shutdown and sets safe mode after a boot loop.
func (d *Diag) BootCheck(ctx context.Context) (string, error) {
	cfg := d.Cfg.Watchdog
	st := d.LoadState()
	now := d.Now().Unix()
	st.UncleanBoots = keep(st.UncleanBoots, now, cfg.RebootWindow)
	result := "clean"
	_, err := os.Stat(d.data("running"))
	unclean := err == nil
	// Mark this boot before the slow capture. If the unit times out during
	// the capture, the next boot still sees only this boot's marker.
	if err := writeAtomic(d.data("running"), []byte(d.bootID()+"\n"), 0o640, "", -1); err != nil {
		return "", err
	}
	if unclean {
		result = "unclean"
		st.UncleanBoots = append(st.UncleanBoots, now)
		watchdogReboot := len(st.Reboots) > 0 && now-st.Reboots[len(st.Reboots)-1] < 900
		if err := d.SaveState(st); err != nil {
			return "", err
		}
		if _, err := d.Capture(ctx, "unclean-shutdown", map[string]any{"watchdog_reboot": watchdogReboot}, true); err != nil {
			d.Log.Error("incident capture", "err", err)
		}
	}
	// These counters belong to the previous boot.
	st.NRestarts, st.Crashes, st.Degraded, st.JournalCursor = -1, nil, false, ""
	if n := len(st.UncleanBoots); n > cfg.MaxReboots {
		if on, _ := d.SafeMode(); !on {
			if err := d.SetSafeMode(true, fmt.Sprintf("boot loop: %d unclean boots", n)); err != nil {
				return "", err
			}
			if _, err := d.Capture(ctx, "boot-loop", map[string]any{"unclean_boots": n}, false); err != nil {
				d.Log.Error("incident capture", "err", err)
			}
			result = "safe-mode"
		}
	}
	return result, d.SaveState(st)
}

// CleanShutdown removes the marker that BootCheck looks for.
func (d *Diag) CleanShutdown() error {
	if err := os.Remove(d.data("running")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Diag) bootID() string {
	b, err := os.ReadFile(d.path("proc/sys/kernel/random/boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
