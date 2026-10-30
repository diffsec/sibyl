package diag

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSwap is a llama-swap with one model, qwen.
type fakeSwap struct {
	mu         sync.Mutex
	healthy    bool
	state      string
	counters   map[string]float64
	activity   []Activity
	uploads    []*http.Request
	uploadBody [][]byte
}

func newFakeSwap(t *testing.T) (*fakeSwap, *httptest.Server) {
	f := &fakeSwap{healthy: true, state: "ready", counters: map[string]float64{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeSwap) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		f.uploads, f.uploadBody = append(f.uploads, r), append(f.uploadBody, b)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"diagnostic_id":%q}`, r.Header.Get("X-Sibyl-Diagnostic-Id"))
		return
	}
	if !f.healthy {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/health":
		_, _ = io.WriteString(w, "OK")
	case "/running":
		_ = json.NewEncoder(w).Encode(map[string]any{"running": []RunningModel{{Model: "qwen", State: f.state}}})
	case "/upstream/qwen/health":
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	case "/upstream/qwen/metrics":
		_, _ = io.WriteString(w, "# HELP llamacpp:prompt_tokens_total x\n")
		for k, v := range f.counters {
			_, _ = fmt.Fprintf(w, "%s %g\n", k, v)
		}
	case "/api/metrics/activity":
		rows := slices.Clone(f.activity)
		if q.Get("order") == "desc" {
			slices.Reverse(rows)
		}
		minID, _ := strconv.Atoi(q.Get("min_id"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		var out []Activity
		for _, a := range rows {
			if a.ID >= minID && len(out) < limit {
				out = append(out, a)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	case "/logs":
		_, _ = io.WriteString(w, "srv log Authorization: Bearer sib-"+strings.Repeat("a", 48)+"\n")
	case "/api/version":
		_, _ = io.WriteString(w, `{"version":"266"}`)
	default:
		http.NotFound(w, r)
	}
}

// fakeRun records commands and answers systemctl show.
type fakeRun struct {
	mu       sync.Mutex
	calls    []string
	active   string
	restarts int
	kernel   string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case strings.Contains(call, "-p ActiveState"):
		return f.active + "\n", nil
	case strings.Contains(call, "-p NRestarts"):
		return strconv.Itoa(f.restarts) + "\n", nil
	case name == "journalctl" && strings.Contains(call, "--show-cursor"):
		return f.kernel + "-- cursor: s=abc\n", nil
	}
	return "", errors.New(name + ": not found")
}

func (f *fakeRun) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func newDiag(t *testing.T, swapURL string) (*Diag, *fakeRun) {
	t.Helper()
	root := t.TempDir()
	fr := &fakeRun{active: "active"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	d := &Diag{Root: root, Run: fr.run, Now: func() time.Time { return now }, Group: -1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := DefaultConfig()
	cfg.Watchdog.LlamaSwapURL = swapURL
	cfg.Watchdog.ProbeTimeout = 2
	cfg.Watchdog.FailuresBeforeAction = 2
	cfg.Watchdog.StallSeconds = 60
	d.Cfg = cfg
	fakeAMD(t, root)
	return d, fr
}

// fakeAMD builds one 7900 XTX in sysfs and debugfs.
func fakeAMD(t *testing.T, root string) {
	t.Helper()
	dev := filepath.Join(root, "sys/devices/pci0000:00/0000:03:00.0")
	hw := filepath.Join(dev, "hwmon/hwmon3")
	files := map[string]string{
		dev + "/vendor": "0x1002", dev + "/device": "0x744c", dev + "/gpu_busy_percent": "97",
		dev + "/mem_info_vram_total": "25753026560", dev + "/mem_info_vram_used": "21474836480",
		hw + "/temp1_label": "edge", hw + "/temp1_input": "65000",
		hw + "/temp2_label": "junction", hw + "/temp2_input": "88000",
		hw + "/power1_average": "325000000", hw + "/fan1_input": "1800",
		root + "/sys/kernel/debug/dri/1/name":               "amdgpu dev=0000:03:00.0 unique=0000:03:00.0",
		root + "/sys/kernel/debug/dri/1/amdgpu_gpu_recover": "",
		root + "/proc/loadavg":                              "1.50 1.20 1.00 2/300 1234",
		root + "/proc/meminfo":                              "MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
	}
	for p, v := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	card := filepath.Join(root, "sys/class/drm/card1")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(card, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestParseMetrics(t *testing.T) {
	m := ParseMetrics("# c\nfoo_total 3\nbar{b=\"2\",a=\"1\"} 4.5\nbad line\n")
	if m["foo_total"] != 3 || m["bar{a=1,b=2}"] != 4.5 || len(m) != 2 {
		t.Fatalf("ParseMetrics = %v", m)
	}
}

func TestThroughputUsesBusySeconds(t *testing.T) {
	prev := Metrics{genTokens: 100, genSeconds: 2}
	cur := Metrics{genTokens: 400, genSeconds: 5}
	got := Throughput(prev, cur)
	if got["gen_tps"] != 100 || len(got) != 1 {
		t.Fatalf("Throughput = %v, want gen_tps 100 only", got)
	}
}

func TestDiagnosticID(t *testing.T) {
	for range 200 {
		id := NewID()
		if !ValidID(id) || !ValidID(strings.ToLower(id)) || len(id) != 13 {
			t.Fatalf("%s is not valid", id)
		}
		raw := strings.ReplaceAll(strings.TrimPrefix(id, "SBL-"), "-", "")
		for i := range 7 {
			for _, c := range idAlphabet {
				if byte(c) == raw[i] {
					continue
				}
				typo := raw[:i] + string(c) + raw[i+1:]
				if ValidID(typo) {
					t.Fatalf("typo %s of %s passes the check", typo, raw)
				}
			}
		}
	}
}

func TestRedactor(t *testing.T) {
	r := Redactor{Salt: []byte("salt"), Hostname: "gpubox"}
	in := "key sib-" + strings.Repeat("f", 48) + " Authorization: Bearer abc.def.ghi hf_" + strings.Repeat("x", 30) +
		" from 192.168.1.20 to 127.0.0.1 and 10.89.0.4 mac aa:bb:cc:dd:ee:ff on gpubox admin@example.com" +
		` password=hunter2 "api_key": "s3cret"`
	out := r.Text(in)
	for _, leak := range []string{"sib-fff", "abc.def.ghi", "hf_xxx", "192.168.1.20", "aa:bb:cc", "gpubox",
		"admin@example.com", "hunter2", "s3cret"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked in %q", leak, out)
		}
	}
	for _, kept := range []string{"127.0.0.1", "10.89.0.4"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q was removed from %q", kept, out)
		}
	}
	tok := r.Text("192.168.1.20")
	if r.Text("from 192.168.1.20") != "from "+tok || tok == r.Text("192.168.1.21") {
		t.Error("the address token must be stable per address and differ between addresses")
	}
}

func TestDecideLadder(t *testing.T) {
	cfg := DefaultConfig().Watchdog
	now := int64(100000)
	st := State{}
	steps := []struct {
		setup func()
		want  string
	}{
		{func() {}, ActRestart},
		{func() { st.Restarts = []int64{now - 10, now - 10, now - 10} }, ActGPUReset},
		{func() { st.GPUResets = []int64{now - 10} }, ActReboot},
		{func() { st.Reboots = []int64{now - 100, now - 50} }, ActDegraded},
	}
	for i, s := range steps {
		s.setup()
		if got := Decide(st, cfg, now, ProbAPIUnresponsive, true); got != s.want {
			t.Fatalf("step %d: Decide = %s, want %s", i, got, s.want)
		}
	}
	if got := Decide(State{Restarts: []int64{now - 1000, now - 1000, now - 1000}}, cfg, now, ProbAPIUnresponsive, true); got != ActRestart {
		t.Errorf("old restarts must leave the window: got %s", got)
	}
	if got := Decide(State{}, cfg, now, ProbCrashLoop, false); got != ActReboot {
		t.Errorf("a crash loop on a non-AMD host must skip restart and reset: got %s", got)
	}
}

func TestSamplerRecordsSpeedAndRequests(t *testing.T) {
	fs, srv := newFakeSwap(t)
	d, _ := newDiag(t, srv.URL)
	s := NewSampler(d)
	ctx := context.Background()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	fs.counters = Metrics{genTokens: 500, genSeconds: 10, promptTokens: 2000, promptSeconds: 1}
	for i := 1; i <= 3; i++ {
		a := Activity{ID: i, Timestamp: d.Now(), Model: "qwen", StatusCode: 200}
		a.Tokens.TokensPerSecond = 40 + float64(i)
		fs.activity = append(fs.activity, a)
	}
	fs.mu.Unlock()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	samples, err := d.Samples(d.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var gen, busy []Sample
	for _, x := range samples {
		switch x.Metric {
		case "gen_tps":
			gen = append(gen, x)
		case "gpu_busy_percent":
			busy = append(busy, x)
		}
	}
	if len(gen) != 1 || gen[0].Value != 50 || gen[0].Labels != "model=qwen" {
		t.Errorf("gen_tps samples = %+v, want one of 50 for qwen", gen)
	}
	if len(busy) != 2 || busy[0].Labels != "gpu=0" || busy[0].Value != 97 {
		t.Errorf("gpu_busy_percent samples = %+v", busy)
	}
	count := func() int {
		reqs, err := d.Requests(d.Now().Add(-time.Hour), "")
		if err != nil {
			t.Fatal(err)
		}
		return len(reqs)
	}
	if n := count(); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	if err := s.Once(ctx); err != nil || count() != 3 {
		t.Fatalf("a sample with no new rows must not duplicate: %d, %v", count(), err)
	}
	// llama-swap restarted: its IDs begin again at 1.
	fs.mu.Lock()
	fs.activity = fs.activity[:1]
	fs.mu.Unlock()
	_ = s.Once(ctx)
	_ = s.Once(ctx)
	if n := count(); n != 4 {
		t.Fatalf("after a llama-swap restart, requests = %d, want 4", n)
	}
	reqs, _ := d.Requests(d.Now().Add(-time.Hour), "")
	p := Perf(reqs)
	if len(p) != 1 || p[0].Requests != 4 || *p[0].GenTPSP50 != 41.5 {
		t.Errorf("Perf = %+v, want 4 requests with p50 41.5", p)
	}
}

func TestWatchdogStallAndRestart(t *testing.T) {
	fs, srv := newFakeSwap(t)
	d, fr := newDiag(t, srv.URL)
	fs.counters = Metrics{"llamacpp:requests_processing": 1, "llamacpp:n_decode_total": 10}
	w := NewWatchdog(d)
	ctx := context.Background()
	if p := w.checkModels(ctx, 1000); p != nil {
		t.Fatalf("first probe: %v", p)
	}
	if p := w.checkModels(ctx, 1030); p != nil {
		t.Fatalf("30 s without progress is not a stall yet: %v", p)
	}
	p := w.checkModels(ctx, 1100)
	if p == nil || p.Name != ProbStalled {
		t.Fatalf("70 s without progress: got %v, want %s", p, ProbStalled)
	}
	fs.mu.Lock()
	fs.counters["llamacpp:n_decode_total"] = 11
	fs.mu.Unlock()
	if p := w.checkModels(ctx, 1130); p != nil {
		t.Fatalf("progress must clear the stall: %v", p)
	}

	// Two failed health checks lead to an incident and a restart.
	fs.mu.Lock()
	fs.healthy = false
	fs.mu.Unlock()
	w.Tick(ctx)
	if fr.called("systemctl --user -M sibyl@ restart") {
		t.Fatal("one failure must not restart")
	}
	w.Tick(ctx)
	if !fr.called("systemctl --user -M sibyl@ restart --no-block llama-swap.service") {
		t.Fatalf("no restart after two failures; calls: %v", fr.calls)
	}
	if st := d.LoadState(); st.LastAction != ActRestart || len(st.Restarts) != 1 {
		t.Errorf("state = %+v", st)
	}
	if n := len(d.Incidents()); n != 1 {
		t.Errorf("incidents = %d, want 1", n)
	}
}

func TestWatchdogLeavesAStoppedOrStartingUnitAlone(t *testing.T) {
	fs, srv := newFakeSwap(t)
	d, fr := newDiag(t, srv.URL)
	fs.healthy = false
	w := NewWatchdog(d)
	for _, state := range []string{"inactive", "activating", "inactive"} {
		fr.active = state
		w.Tick(context.Background())
	}
	if fr.called("systemctl --user -M sibyl@ restart") {
		t.Fatal("the watchdog restarted a unit that was stopped or starting")
	}
}

func TestWatchdogRecordsGPUFaults(t *testing.T) {
	_, srv := newFakeSwap(t)
	d, fr := newDiag(t, srv.URL)
	fr.kernel = "amdgpu 0000:03:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=3\n"
	w := NewWatchdog(d)
	w.Tick(context.Background())
	inc := d.Incidents()
	if len(inc) != 1 || !strings.HasSuffix(inc[0], "-"+ProbGPUFault) {
		t.Fatalf("incidents = %v, want one gpu-fault", inc)
	}
	if w.st.JournalCursor != "s=abc" {
		t.Errorf("cursor = %q", w.st.JournalCursor)
	}
	if gpuFault.MatchString("amdgpu: SMU driver if version matched") {
		t.Error("a normal amdgpu line matched")
	}
}

func TestGPUResetFindsTheDebugfsFile(t *testing.T) {
	_, srv := newFakeSwap(t)
	d, fr := newDiag(t, srv.URL)
	w := NewWatchdog(d)
	if err := w.gpuReset(context.Background()); err == nil {
		t.Fatal("the fake runner fails cat, so the reset must fail")
	}
	want := "cat " + filepath.Join(d.Root, "sys/kernel/debug/dri/1/amdgpu_gpu_recover")
	if !fr.called(want) {
		t.Fatalf("calls = %v, want %q", fr.calls, want)
	}
}

func TestBootCheckEntersSafeModeAfterABootLoop(t *testing.T) {
	_, srv := newFakeSwap(t)
	d, _ := newDiag(t, srv.URL)
	ctx := context.Background()
	if res, err := d.BootCheck(ctx); err != nil || res != "clean" {
		t.Fatalf("first boot = %s, %v", res, err)
	}
	var got []string
	for range 3 { // the marker is never removed: three crashes
		res, err := d.BootCheck(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, res)
	}
	if want := []string{"unclean", "unclean", "safe-mode"}; !slices.Equal(got, want) {
		t.Fatalf("boot checks = %v, want %v", got, want)
	}
	if on, _ := d.SafeMode(); !on {
		t.Fatal("safe mode is off")
	}
	if err := d.SetSafeMode(false, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.CleanShutdown(); err != nil {
		t.Fatal(err)
	}
	if res, _ := d.BootCheck(ctx); res != "clean" {
		t.Errorf("after a clean shutdown: %s", res)
	}
}

func TestBundleIsRedactedAndUploadNeedsConsent(t *testing.T) {
	fs, srv := newFakeSwap(t)
	d, _ := newDiag(t, srv.URL)
	ctx := context.Background()
	dir, err := d.Capture(ctx, "manual", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.BuildBundle(dir, "box at 192.168.7.7 hung")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(b.ID) {
		t.Fatalf("bad ID %s", b.ID)
	}
	files := readTarGz(t, b.Path)
	logs, ok := files[b.ID+"/incident/llama-swap-logs.txt"]
	if !ok {
		t.Fatalf("bundle files: %v", slices.Sorted(maps.Keys(files)))
	}
	if strings.Contains(logs, "sib-aaaa") {
		t.Errorf("the API key leaked: %s", logs)
	}
	if strings.Contains(files[b.ID+"/manifest.json"], "192.168.7.7") {
		t.Error("the note was not redacted")
	}

	if _, err := d.Upload(ctx, b.Path); !errors.Is(err, ErrConsentOff) {
		t.Fatalf("upload with consent off: %v", err)
	}
	if _, err := d.SaveConsent(ConsentManual, srv.URL); err != nil {
		t.Fatal(err)
	}
	id, err := d.Upload(ctx, b.Path)
	if err != nil || id != b.ID {
		t.Fatalf("upload = %s, %v", id, err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.uploads) != 1 || fs.uploads[0].Header.Get("X-Sibyl-Diagnostic-Id") != b.ID {
		t.Fatalf("uploads = %d", len(fs.uploads))
	}
	raw, _ := os.ReadFile(b.Path)
	if string(fs.uploadBody[0]) != string(raw) {
		t.Error("the uploaded body differs from the bundle")
	}
}

func TestUploadPendingOnlyInAutomaticMode(t *testing.T) {
	fs, srv := newFakeSwap(t)
	d, _ := newDiag(t, srv.URL)
	ctx := context.Background()
	dir, _ := d.Capture(ctx, "manual", nil, false)
	b, err := d.BuildBundle(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	d.MarkPending(b.Path)
	if _, err := d.SaveConsent(ConsentManual, srv.URL); err != nil {
		t.Fatal(err)
	}
	if sent, _ := d.UploadPending(ctx); len(sent) != 0 {
		t.Fatal("manual mode sent a queued bundle")
	}
	if _, err := d.SaveConsent(ConsentAutomatic, ""); err != nil {
		t.Fatal(err)
	}
	sent, err := d.UploadPending(ctx)
	if err != nil || len(sent) != 1 {
		t.Fatalf("sent = %v, %v", sent, err)
	}
	if _, err := os.Stat(b.Path + ".pending"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the pending marker is still there")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.uploads) != 1 {
		t.Errorf("uploads = %d", len(fs.uploads))
	}
}

func TestCheckEndpoint(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://diag.example.com": true, "http://127.0.0.1:9000": true,
		"http://diag.example.com": false, "ftp://x": false, "diag.example.com": false,
	} {
		if err := CheckEndpoint(url); (err == nil) != ok {
			t.Errorf("CheckEndpoint(%q) = %v", url, err)
		}
	}
}

func TestLoadConfigOverridesKeys(t *testing.T) {
	d, _ := newDiag(t, "http://127.0.0.1:1")
	p := filepath.Join(d.Root, overrideFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("watchdog:\n  max_reboots: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if d.Cfg.Watchdog.MaxReboots != 0 || d.Cfg.Watchdog.ProbeInterval != 30 {
		t.Errorf("config = %+v", d.Cfg.Watchdog)
	}
	if err := os.WriteFile(p, []byte("watchdog: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.LoadConfig(); err == nil {
		t.Error("a broken override must be an error")
	}
}

func readTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}
