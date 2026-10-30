package diag

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An incident is a directory in IncidentDir with what is needed to debug
// one failure: logs, GPU state, the loaded models, recent metrics and the
// hardware inventory. The watchdog captures it before it acts, because a
// restart or a reboot destroys most of that state. Incidents stay on the
// box. Only a bundle copies one out, redacted.

// maxCaptureFile is the largest file one capture step keeps. Larger output
// keeps its tail, where the failure is.
const maxCaptureFile = 20 << 20

// Capture records an incident and returns its directory. A failed step does
// not stop the others. The errors go to capture-errors.txt.
func (d *Diag) Capture(ctx context.Context, reason string, details map[string]any, previousBoot bool) (string, error) {
	now := d.Now().UTC()
	dir := filepath.Join(d.IncidentDir(), now.Format("20060102T150405Z")+"-"+reason)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	var errs []string
	swap := NewLlamaSwap(d.Cfg.Watchdog.LlamaSwapURL, time.Duration(d.Cfg.Watchdog.ProbeTimeout)*time.Second)
	step := func(name string, fn func() (any, error)) {
		// A full capture can take longer than the calling unit's WatchdogSec.
		Notify("WATCHDOG=1")
		v, err := fn()
		if err != nil {
			errs = append(errs, name+": "+err.Error())
		}
		if v != nil {
			if werr := writeCapture(filepath.Join(dir, name), v); werr != nil {
				errs = append(errs, name+": "+werr.Error())
			}
		}
	}
	cmd := func(timeout time.Duration, name string, args ...string) func() (any, error) {
		return func() (any, error) {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out, err := d.Run(cctx, name, args...)
			return out, err
		}
	}
	journal := func(args ...string) func() (any, error) {
		return cmd(time.Minute, "journalctl", append([]string{"--no-pager", "-o", "short-iso-precise"}, args...)...)
	}
	user := func(args ...string) func() (any, error) {
		return cmd(time.Minute, "setpriv", AsSibyl("systemctl", append([]string{"--user", "--no-pager"}, args...)...)...)
	}
	podman := func(args ...string) func() (any, error) {
		return cmd(time.Minute, "setpriv", AsSibyl("podman", args...)...)
	}

	safe, why := d.SafeMode()
	step("incident.json", func() (any, error) {
		return map[string]any{
			"reason": reason, "details": details, "time": now, "boot_id": d.bootID(),
			"device_id": d.DeviceID(), "version": d.readShare("version"),
			"gpu_variant": d.readShare("gpu-variant"), "image_tag": d.readShare("image-tag"),
			"safe_mode": safe, "safe_mode_reason": why,
		}, nil
	})
	if previousBoot {
		step("journal-previous-boot.txt", journal("-b", "-1", "-n", "20000"))
		step("journal-previous-boot-kernel.txt", journal("-k", "-b", "-1", "-n", "20000"))
		step("pstore.txt", func() (any, error) { return d.copyPstore(dir) })
	}
	step("journal-kernel.txt", journal("-k", "-b", "0", "-n", "20000"))
	step("journal-system.txt", journal("-b", "0", "--since", "-1h", "-n", "50000"))
	step("journal-llama-swap.txt", journal("CONTAINER_NAME=llama-swap", "--since", "-2h", "-n", "50000"))
	step("journal-sibyl-user.txt", journal("_UID=1001", "--since", "-1h", "-n", "20000"))
	step("llama-swap-logs.txt", func() (any, error) { return swap.Raw(ctx, "/logs") })
	step("llama-swap-state.json", func() (any, error) { return d.swapSnapshot(ctx, swap), nil })
	step("llama-swap-config.yaml", func() (any, error) { return readUserFile(d.path(llamaSwapConfig), maxConfigBytes) })
	step("units.txt", user("status", "-n", "50", "llama-swap.service", "router.service",
		"open-webui.service", "caddy.service", "system-api.service"))
	step("units-failed.txt", cmd(30*time.Second, "systemctl", "--failed", "--no-pager"))
	step("podman-ps.json", podman("ps", "-a", "--format", "json"))
	step("podman-images.json", podman("images", "--digests", "--format", "json"))
	step("gpu-tools.txt", func() (any, error) { return d.gpuTools(ctx), nil })
	step("hardware.json", func() (any, error) { return d.Inventory(ctx), nil })
	step("watchdog-state.json", func() (any, error) { return d.LoadState(), nil })
	step("requests-1h.csv", func() (any, error) { return d.requestsCSV(now.Add(-time.Hour)) })
	step("samples-1h.jsonl", func() (any, error) { return d.samplesJSONL(now.Add(-time.Hour)) })
	step("perf-24h.json", func() (any, error) {
		reqs, err := d.Requests(now.Add(-24*time.Hour), "")
		return Perf(reqs), err
	})
	if len(errs) > 0 {
		_ = writeCapture(filepath.Join(dir, "capture-errors.txt"), strings.Join(errs, "\n")+"\n")
	}
	d.PruneIncidents()
	return dir, nil
}

func writeCapture(path string, v any) error {
	var b []byte
	switch t := v.(type) {
	case string:
		b = []byte(t)
	case []byte:
		b = t
	default:
		var err error
		if b, err = json.MarshalIndent(v, "", "  "); err != nil {
			return err
		}
	}
	if len(b) > maxCaptureFile {
		b = append([]byte("<truncated, kept the tail>\n"), b[len(b)-maxCaptureFile:]...)
	}
	return os.WriteFile(path, b, 0o640)
}

func (d *Diag) swapSnapshot(ctx context.Context, swap *LlamaSwap) map[string]any {
	out := map[string]any{}
	for key, path := range map[string]string{
		"version": "/api/version", "running": "/running",
		"activity": "/api/metrics/activity?limit=200&sort=id&order=desc",
	} {
		b, err := swap.Raw(ctx, path)
		if err != nil {
			out[key] = "unavailable: " + err.Error()
			continue
		}
		out[key] = json.RawMessage(b)
	}
	running, err := swap.Running(ctx)
	if err != nil {
		return out
	}
	upstream := map[string]any{}
	for _, r := range running {
		if m, err := swap.UpstreamMetrics(ctx, r.Model); err == nil {
			upstream[r.Model] = m
		} else {
			upstream[r.Model] = "unavailable: " + err.Error()
		}
	}
	out["upstream_metrics"] = upstream
	return out
}

func (d *Diag) gpuTools(ctx context.Context) string {
	var parts []string
	for _, c := range [][]string{
		{"amd-smi", "static", "--json"}, {"amd-smi", "metric", "--json"},
		{"rocm-smi", "--showallinfo"}, {"nvidia-smi", "-q"},
	} {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		out, err := d.Run(cctx, c[0], c[1:]...)
		cancel()
		if err == nil || out != "" {
			parts = append(parts, "$ "+strings.Join(c, " ")+"\n"+out)
		}
	}
	if len(parts) == 0 {
		return "no GPU tool is installed on the host\n"
	}
	return strings.Join(parts, "\n\n")
}

// copyPstore copies the newest crash record that systemd-pstore archived.
// systemd-pstore writes dmesg.txt files, so the record is text.
func (d *Diag) copyPstore(dir string) (string, error) {
	entries, err := os.ReadDir(d.path(pstoreDir))
	if err != nil || len(entries) == 0 {
		return "no pstore record\n", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	latest := filepath.Join(d.path(pstoreDir), entries[len(entries)-1].Name())
	target := filepath.Join(dir, "pstore")
	err = filepath.WalkDir(latest, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !e.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(latest, p)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = e.Name()
		}
		return copyFile(p, filepath.Join(target, rel))
	})
	return "copied " + filepath.Base(latest) + "\n", err
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxCaptureFile)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (d *Diag) requestsCSV(since time.Time) (string, error) {
	reqs, err := d.Requests(since, "")
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"time", "model", "status", "input_tokens", "cached_tokens", "output_tokens",
		"prompt_tps", "gen_tps", "duration_ms", "error"})
	for _, r := range reqs {
		_ = w.Write([]string{
			time.Unix(r.T, 0).UTC().Format(time.RFC3339), r.Model, strconv.Itoa(r.Status),
			strconv.Itoa(r.InputTokens), strconv.Itoa(r.CachedTokens), strconv.Itoa(r.OutputTokens),
			fmt.Sprintf("%.2f", r.PromptTPS), fmt.Sprintf("%.2f", r.GenTPS), strconv.Itoa(r.DurationMS), r.Error,
		})
	}
	w.Flush()
	return b.String(), err
}

func (d *Diag) samplesJSONL(since time.Time) (string, error) {
	samples, err := d.Samples(since)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, s := range samples {
		_ = enc.Encode(s)
	}
	return b.String(), err
}

// Incidents lists the incident directory names, oldest first.
func (d *Diag) Incidents() []string {
	entries, err := os.ReadDir(d.IncidentDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// PruneIncidents keeps at most MaxIncidents directories and MaxIncidentBytes.
func (d *Diag) PruneIncidents() {
	names := d.Incidents()
	sizes := make([]int64, len(names))
	var total int64
	for i, n := range names {
		_ = filepath.WalkDir(filepath.Join(d.IncidentDir(), n), func(_ string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if info, err := e.Info(); err == nil {
					sizes[i] += info.Size()
				}
			}
			return nil
		})
		total += sizes[i]
	}
	c := d.Cfg.Capture
	for i := 0; i < len(names) && (len(names)-i > c.MaxIncidents || total > c.MaxIncidentBytes); i++ {
		_ = os.RemoveAll(filepath.Join(d.IncidentDir(), names[i]))
		total -= sizes[i]
	}
}
