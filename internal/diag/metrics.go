package diag

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/diffsec/sibyl/internal/gpu"
)

// The store is one JSON lines file per kind and UTC day in MetricsDir:
// samples-20261030.jsonl and requests-20261030.jsonl. Files older than
// RetentionDays are deleted. The host build has no cgo, so no SQLite.

// Sample is one reading of one series.
type Sample struct {
	T      int64   `json:"t"`
	Metric string  `json:"m"`
	Labels string  `json:"l,omitempty"`
	Value  float64 `json:"v"`
}

// Request is one llama-swap request row: token counts and speeds, never content.
type Request struct {
	T            int64   `json:"t"`
	Model        string  `json:"model"`
	Path         string  `json:"path,omitempty"`
	Status       int     `json:"status"`
	InputTokens  int     `json:"input_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	OutputTokens int     `json:"output_tokens"`
	PromptTPS    float64 `json:"prompt_tps"`
	GenTPS       float64 `json:"gen_tps"`
	DurationMS   int     `json:"duration_ms"`
	Error        string  `json:"error,omitempty"`
	SwapID       int     `json:"swap_id"`
}

func (d *Diag) dayFile(kind string, t time.Time) string {
	return filepath.Join(d.MetricsDir(), kind+"-"+t.UTC().Format("20060102")+".jsonl")
}

func (d *Diag) appendLines(kind string, now time.Time, rows []any) error {
	if len(rows) == 0 {
		return nil
	}
	if err := os.MkdirAll(d.MetricsDir(), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(d.dayFile(kind, now), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readLines calls fn for every line of kind with a time at or after since.
func readLines[T any](d *Diag, kind string, since time.Time, fn func(T)) error {
	files, err := filepath.Glob(filepath.Join(d.MetricsDir(), kind+"-*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	first := kind + "-" + since.UTC().Format("20060102") + ".jsonl"
	for _, p := range files {
		if filepath.Base(p) < first {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var v T
			if json.Unmarshal(sc.Bytes(), &v) == nil {
				fn(v)
			}
		}
		_ = f.Close()
	}
	return nil
}

// Requests returns the request rows since a time, optionally for one model.
func (d *Diag) Requests(since time.Time, model string) ([]Request, error) {
	var out []Request
	err := readLines(d, "requests", since, func(r Request) {
		if r.T >= since.Unix() && (model == "" || r.Model == model) {
			out = append(out, r)
		}
	})
	return out, err
}

// Samples returns the samples since a time.
func (d *Diag) Samples(since time.Time) ([]Sample, error) {
	var out []Sample
	err := readLines(d, "samples", since, func(s Sample) {
		if s.T >= since.Unix() {
			out = append(out, s)
		}
	})
	return out, err
}

// PruneMetrics deletes the day files older than RetentionDays.
func (d *Diag) PruneMetrics() {
	files, _ := filepath.Glob(filepath.Join(d.MetricsDir(), "*.jsonl"))
	cut := d.Now().UTC().AddDate(0, 0, -d.Cfg.Capture.RetentionDays).Format("20060102")
	for _, p := range files {
		name := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if i := strings.LastIndexByte(name, '-'); i >= 0 && name[i+1:] < cut {
			_ = os.Remove(p)
		}
	}
}

// llama-server counters that give throughput when two readings are compared.
const (
	genTokens     = "llamacpp:tokens_predicted_total"
	genSeconds    = "llamacpp:tokens_predicted_seconds_total"
	promptTokens  = "llamacpp:prompt_tokens_total"
	promptSeconds = "llamacpp:prompt_seconds_total"
)

// Throughput gives tokens per second between two llama-server readings. The
// counters cover busy time only, so the rate is the speed while the server
// works, not tokens divided by wall-clock time.
func Throughput(prev, cur Metrics) map[string]float64 {
	out := map[string]float64{}
	for name, keys := range map[string][2]string{
		"gen_tps":    {genTokens, genSeconds},
		"prompt_tps": {promptTokens, promptSeconds},
	} {
		dt := cur[keys[0]] - prev[keys[0]]
		ds := cur[keys[1]] - prev[keys[1]]
		if dt > 0 && ds > 0 {
			out[name] = dt / ds
		}
	}
	return out
}

// Sampler takes one reading every SampleInterval.
type Sampler struct {
	d      *Diag
	swap   *LlamaSwap
	gpus   []gpu.GPU
	prev   map[string]Metrics
	lastID int
}

type cursor struct {
	SwapID int `json:"swap_id"`
}

// NewSampler detects the GPUs once and restores the request cursor.
func NewSampler(d *Diag) *Sampler {
	gpus, err := gpu.Detect(d.path("sys"))
	if err != nil {
		d.Log.Warn("detect GPUs", "err", err)
	}
	s := &Sampler{d: d, swap: NewLlamaSwap(d.Cfg.Watchdog.LlamaSwapURL, 5*time.Second), gpus: gpus,
		prev: map[string]Metrics{}}
	var c cursor
	if b, err := os.ReadFile(filepath.Join(d.MetricsDir(), "cursor.json")); err == nil && json.Unmarshal(b, &c) == nil {
		s.lastID = c.SwapID
	}
	return s
}

// Once takes one reading and copies new request rows.
func (s *Sampler) Once(ctx context.Context) error {
	now := s.d.Now()
	t := now.Unix()
	var rows []any
	add := func(metric, labels string, v float64) {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			rows = append(rows, Sample{T: t, Metric: metric, Labels: labels, Value: v})
		}
	}
	for k, v := range s.d.hostSample() {
		add("host_"+k, "", v)
	}
	nv := s.nvidia(ctx)
	for _, g := range s.gpus {
		reading := gpu.Sample(s.d.path("sys"), g)
		if g.Vendor == "nvidia" {
			reading = nv[strings.ToLower(g.PCIAddr)]
		}
		for k, v := range reading {
			add("gpu_"+k, "gpu="+strconv.Itoa(g.Index), v)
		}
	}
	s.inference(ctx, add)
	if err := s.d.appendLines("samples", now, rows); err != nil {
		return err
	}
	return s.copyActivity(ctx, now)
}

func (s *Sampler) inference(ctx context.Context, add func(string, string, float64)) {
	running, err := s.swap.Running(ctx)
	if err != nil {
		add("swap_up", "", 0)
		return
	}
	add("swap_up", "", 1)
	add("swap_models_running", "", float64(len(running)))
	seen := map[string]bool{}
	for _, r := range running {
		if r.State != "ready" {
			continue
		}
		seen[r.Model] = true
		labels := "model=" + r.Model
		cur, err := s.swap.UpstreamMetrics(ctx, r.Model)
		if err != nil {
			add("model_metrics_up", labels, 0)
			continue
		}
		add("model_metrics_up", labels, 1)
		for k, v := range cur {
			if strings.HasPrefix(k, "llamacpp:") {
				add(k, labels, v)
			}
		}
		if prev, ok := s.prev[r.Model]; ok {
			for k, v := range Throughput(prev, cur) {
				add(k, labels, v)
			}
		}
		s.prev[r.Model] = cur
	}
	for m := range s.prev {
		if !seen[m] {
			delete(s.prev, m)
		}
	}
}

// copyActivity appends llama-swap request rows above the cursor. llama-swap
// keeps its rows in memory, so its IDs start again at 1 after a restart.
func (s *Sampler) copyActivity(ctx context.Context, now time.Time) error {
	acts, err := s.swap.Activities(ctx, s.lastID+1, 999, "asc")
	if err != nil {
		return nil // llama-swap is down; the samples say so
	}
	if len(acts) == 0 {
		newest, err := s.swap.Activities(ctx, 0, 1, "desc")
		if err == nil && len(newest) > 0 && newest[0].ID < s.lastID {
			s.lastID = 0
			return s.saveCursor()
		}
		return nil
	}
	rows := make([]any, 0, len(acts))
	for _, a := range acts {
		rows = append(rows, Request{
			T: a.Timestamp.Unix(), Model: a.Model, Path: a.ReqPath, Status: a.StatusCode,
			InputTokens: a.Tokens.Input, CachedTokens: a.Tokens.Cached, OutputTokens: a.Tokens.Output,
			PromptTPS: a.Tokens.PromptPerSecond, GenTPS: a.Tokens.TokensPerSecond,
			DurationMS: a.DurationMS, Error: truncate(a.Error, 300), SwapID: a.ID,
		})
		s.lastID = max(s.lastID, a.ID)
	}
	if err := s.d.appendLines("requests", now, rows); err != nil {
		return err
	}
	return s.saveCursor()
}

func (s *Sampler) saveCursor() error {
	return s.d.writeJSON(filepath.Join(s.d.MetricsDir(), "cursor.json"), cursor{SwapID: s.lastID}, 0o640)
}

const nvidiaFields = "pci.bus_id,utilization.gpu,utilization.memory,memory.used,memory.total," +
	"temperature.gpu,power.draw,power.limit,fan.speed,clocks.sm,clocks.mem"

// nvidia reads every NVIDIA card with one nvidia-smi call, keyed by the
// lower-case PCI address in sysfs form.
func (s *Sampler) nvidia(ctx context.Context) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	has := false
	for _, g := range s.gpus {
		has = has || g.Vendor == "nvidia"
	}
	if !has {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, err := s.d.Run(ctx, "nvidia-smi", "--query-gpu="+nvidiaFields, "--format=csv,noheader,nounits")
	if err != nil {
		return out
	}
	names := []string{"busy_percent", "mem_busy_percent", "vram_used_bytes", "vram_total_bytes",
		"temp_edge_c", "power_w", "power_cap_w", "fan_percent", "sclk_mhz", "mclk_mhz"}
	for _, line := range strings.Split(text, "\n") {
		cols := strings.Split(line, ",")
		if len(cols) != len(names)+1 {
			continue
		}
		// nvidia-smi prints 00000000:03:00.0, and sysfs 0000:03:00.0.
		addr := strings.ToLower(strings.TrimSpace(cols[0]))
		if len(addr) > 12 {
			addr = addr[len(addr)-12:]
		}
		r := map[string]float64{}
		for i, n := range names {
			v, err := strconv.ParseFloat(strings.TrimSpace(cols[i+1]), 64)
			if err != nil {
				continue
			}
			if strings.HasPrefix(n, "vram") {
				v *= 1 << 20
			}
			r[n] = v
		}
		out[addr] = r
	}
	return out
}

func (d *Diag) hostSample() map[string]float64 {
	out := map[string]float64{}
	if b, err := os.ReadFile(d.path("proc", "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 2 {
			out["load1"], _ = strconv.ParseFloat(f[0], 64)
			out["load5"], _ = strconv.ParseFloat(f[1], 64)
		}
	}
	mem := meminfo(d.path("proc", "meminfo"))
	if total, ok := mem["MemTotal"]; ok {
		out["mem_total_bytes"] = total
		out["mem_available_bytes"] = mem["MemAvailable"]
		out["swap_used_bytes"] = mem["SwapTotal"] - mem["SwapFree"]
	}
	var st unix.Statfs_t
	if unix.Statfs(d.path("var/lib/sibyl"), &st) == nil {
		out["state_free_bytes"] = float64(uint64(st.Bavail) * uint64(st.Bsize))
	}
	return out
}

// meminfo reads /proc/meminfo in bytes.
func meminfo(path string) map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		f := strings.Fields(rest)
		if !ok || len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

// RunMetrics samples until ctx ends.
func (d *Diag) RunMetrics(ctx context.Context) error {
	s := NewSampler(d)
	Notify("READY=1")
	interval := time.Duration(max(1, d.Cfg.Capture.SampleInterval)) * time.Second
	lastPrune, lastPerf := time.Time{}, time.Time{}
	for {
		start := time.Now()
		if err := s.Once(ctx); err != nil {
			d.Log.Error("sample", "err", err)
		}
		if time.Since(lastPerf) >= time.Minute {
			if err := d.WriteOut(); err != nil {
				d.Log.Warn("write status", "err", err)
			}
			lastPerf = time.Now()
		}
		if time.Since(lastPrune) >= time.Hour {
			d.PruneMetrics()
			lastPrune = time.Now()
		}
		Notify("WATCHDOG=1")
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-time.After(max(time.Second, interval-time.Since(start))):
		}
	}
}

// ModelPerf is the speed summary of one model.
type ModelPerf struct {
	Model        string   `json:"model"`
	Requests     int      `json:"requests"`
	Errors       int      `json:"errors"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	GenTPSP50    *float64 `json:"gen_tps_p50"`
	GenTPSP5     *float64 `json:"gen_tps_p5"`
	PromptTPSP50 *float64 `json:"prompt_tps_p50"`
}

// Perf summarises the request rows per model. p5 is the slow tail: 95% of
// requests generated faster than it.
func Perf(reqs []Request) []ModelPerf {
	by := map[string][]Request{}
	for _, r := range reqs {
		by[r.Model] = append(by[r.Model], r)
	}
	out := make([]ModelPerf, 0, len(by))
	for model, rs := range by {
		p := ModelPerf{Model: model, Requests: len(rs)}
		var gen, pp []float64
		for _, r := range rs {
			if r.Status >= 400 || r.Error != "" {
				p.Errors++
			}
			p.InputTokens += r.InputTokens
			p.OutputTokens += r.OutputTokens
			if r.GenTPS > 0 {
				gen = append(gen, r.GenTPS)
			}
			if r.PromptTPS > 0 {
				pp = append(pp, r.PromptTPS)
			}
		}
		p.GenTPSP50, p.GenTPSP5, p.PromptTPSP50 = percentile(gen, 50), percentile(gen, 5), percentile(pp, 50)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// percentile interpolates linearly between the closest ranks.
func percentile(vs []float64, q float64) *float64 {
	if len(vs) == 0 {
		return nil
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	pos := q / 100 * float64(len(s)-1)
	lo := int(pos)
	v := s[lo]
	if lo+1 < len(s) {
		v += (pos - float64(lo)) * (s[lo+1] - s[lo])
	}
	v = math.Round(v*100) / 100
	return &v
}

// WriteOut writes status.json and perf.json for the System API.
func (d *Diag) WriteOut() error {
	reqs, err := d.Requests(d.Now().Add(-24*time.Hour), "")
	if err != nil {
		return err
	}
	if err := d.writeOut("perf.json", map[string]any{
		"updated": d.Now().UTC(), "window_hours": 24, "models": Perf(reqs),
	}); err != nil {
		return err
	}
	return d.writeOut("status.json", d.Status())
}

// Status is what `sibyl-diag status` prints and status.json holds.
func (d *Diag) Status() map[string]any {
	st := d.LoadState()
	safe, why := d.SafeMode()
	c := d.LoadConsent()
	incidents := d.Incidents()
	latest := ""
	if len(incidents) > 0 {
		latest = incidents[len(incidents)-1]
	}
	return map[string]any{
		"updated":   d.Now().UTC(),
		"version":   d.readShare("version"),
		"device_id": d.DeviceID(),
		"consent":   c.Mode,
		"endpoint":  d.Endpoint(),
		"safe_mode": map[string]any{"on": safe, "reason": why},
		"watchdog": map[string]any{
			"degraded": st.Degraded, "last_problem": st.LastProblem, "last_action": st.LastAction,
			"restarts": len(st.Restarts), "reboots": len(st.Reboots), "unclean_boots": len(st.UncleanBoots),
		},
		"incidents":       len(incidents),
		"latest_incident": latest,
	}
}

// FormatTPS prints an optional speed.
func FormatTPS(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}
