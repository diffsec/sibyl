package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/hostctl"
)

const token = "test-token"

type fixture struct {
	server *Server
	srv    *httptest.Server
	paths  Paths
	run    string
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	mkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFixture builds a fake host with two AMD GPUs and two models.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		Config:    filepath.Join(root, "config"),
		Models:    filepath.Join(root, "models"),
		DropIn:    filepath.Join(root, "dropin"),
		SysfsRoot: filepath.Join(root, "sys"),
	}
	run := filepath.Join(root, "run")
	for _, d := range []string{filepath.Join(p.Config, "llama-swap"), p.Models, p.DropIn, filepath.Join(run, "requests")} {
		mkdir(t, d)
	}
	for i, addr := range []string{"0000:03:00.0", "0000:83:00.0"} {
		dev := filepath.Join(p.SysfsRoot, "devices", addr)
		write(t, filepath.Join(dev, "vendor"), "0x1002")
		card := filepath.Join(p.SysfsRoot, "class", "drm", "card"+string(rune('0'+i)))
		mkdir(t, card)
		if err := os.Symlink(dev, filepath.Join(card, "device")); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(p.Models, "chat-14b.gguf"), "x")
	write(t, filepath.Join(p.Models, "big-00001-of-00002.gguf"), "x")
	write(t, filepath.Join(p.Models, "big-00002-of-00002.gguf"), "x")

	s := &Server{
		Paths:    p,
		Host:     hostctl.New(run),
		Token:    token,
		Registry: "ghcr.io/ihavespoons",
		ImageTag: "test",
		Version:  "test",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &fixture{server: s, srv: srv, paths: p, run: run}
}

func (f *fixture) do(t *testing.T, method, path, body string, auth bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (f *fixture) requests(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.run, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		// strip the timestamp prefix
		names = append(names, e.Name()[strings.Index(e.Name(), ".")+1:])
	}
	sort.Strings(names)
	return names
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.do(t, "GET", "/health", "", false); code != http.StatusOK {
		t.Errorf("/health without token = %d, want 200", code)
	}
	if code, _ := f.do(t, "GET", "/gpus", "", false); code != http.StatusUnauthorized {
		t.Errorf("/gpus without token = %d, want 401", code)
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/gpus", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/gpus with wrong token = %d, want 401", resp.StatusCode)
	}
}

func TestPutHardwareAppliesConfig(t *testing.T) {
	f := newFixture(t)

	code, body := f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[0,1],"layout":"span"}`, true)
	if code != http.StatusOK {
		t.Fatalf("PUT /hardware = %d %v", code, body)
	}
	if body["models"].(float64) != 2 || body["backend_changed"] != true {
		t.Errorf("apply result = %v", body)
	}

	cfg, err := os.ReadFile(filepath.Join(f.paths.Config, "llama-swap", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "--device ROCm0,ROCm1") {
		t.Errorf("llama-swap config does not span both GPUs:\n%s", cfg)
	}
	dropIn, err := os.ReadFile(filepath.Join(f.paths.DropIn, "backend.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dropIn), "HIP_VISIBLE_DEVICES=0,1") {
		t.Errorf("drop-in = %s", dropIn)
	}
	if got := f.requests(t); len(got) != 2 || got[0] != "reload.all" || got[1] != "restart.llama-swap" {
		t.Errorf("requests = %v", got)
	}

	// Applying again with no change must not touch the config or restart llama-swap.
	cfgPath := filepath.Join(f.paths.Config, "llama-swap", "config.yaml")
	before, _ := os.Stat(cfgPath)
	if code, body := f.do(t, "POST", "/apply", "", true); code != http.StatusOK || body["backend_changed"] != false || body["config_changed"] != false {
		t.Errorf("second apply = %d %v", code, body)
	}
	if after, _ := os.Stat(cfgPath); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged apply rewrote config.yaml")
	}
	if got := f.requests(t); len(got) != 2 {
		t.Errorf("second apply queued more requests: %v", got)
	}
}

func TestPutHardwareRejectsUnknownGPU(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[2],"layout":"span"}`, true); code != http.StatusUnprocessableEntity {
		t.Errorf("code = %d, want 422", code)
	}
	if code, _ := f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[0],"layout":"span","extra":1}`, true); code != http.StatusBadRequest {
		t.Errorf("unknown field: code = %d, want 400", code)
	}
}

func TestModelSettingsAndDelete(t *testing.T) {
	f := newFixture(t)
	f.do(t, "PUT", "/hardware", `{"backend":"vulkan","gpus":[0,1],"layout":"span"}`, true)

	if code, b := f.do(t, "PUT", "/models/chat-14b", `{"layout":"gpu:1","ctx_size":16384}`, true); code != http.StatusOK {
		t.Fatalf("PUT model = %d %v", code, b)
	}
	cfg, _ := os.ReadFile(filepath.Join(f.paths.Config, "llama-swap", "config.yaml"))
	if !strings.Contains(string(cfg), "--ctx-size 16384 --split-mode none --device Vulkan1") {
		t.Errorf("config:\n%s", cfg)
	}
	if code, _ := f.do(t, "PUT", "/models/chat-14b", `{"args":["--port=1"]}`, true); code != http.StatusUnprocessableEntity {
		t.Errorf("reserved arg: code = %d, want 422", code)
	}
	if code, _ := f.do(t, "PUT", "/models/missing", `{}`, true); code != http.StatusNotFound {
		t.Errorf("missing model: code = %d, want 404", code)
	}

	if code, b := f.do(t, "DELETE", "/models/big", "", true); code != http.StatusOK {
		t.Fatalf("DELETE = %d %v", code, b)
	}
	left, _ := filepath.Glob(filepath.Join(f.paths.Models, "big-*"))
	if len(left) != 0 {
		t.Errorf("shards left after delete: %v", left)
	}
	if _, err := os.Stat(filepath.Join(f.paths.Models, "chat-14b.gguf")); err != nil {
		t.Errorf("delete removed the wrong model: %v", err)
	}
}

func TestRestartService(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.do(t, "POST", "/services/open-webui/restart", "", true); code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", code)
	}
	if code, _ := f.do(t, "POST", "/services/sshd/restart", "", true); code != http.StatusBadRequest {
		t.Errorf("sshd: code = %d, want 400", code)
	}
	if got := f.requests(t); len(got) != 1 || got[0] != "restart.open-webui" {
		t.Errorf("requests = %v", got)
	}
}

// With no hardware.yaml, the first apply must match the image default
// drop-in and leave llama-swap alone.
func TestFirstApplyDoesNotRestart(t *testing.T) {
	f := newFixture(t)
	code, body := f.do(t, "POST", "/apply", "", true)
	if code != http.StatusOK || body["backend_changed"] != false {
		t.Fatalf("first apply = %d %v", code, body)
	}
	if got := f.requests(t); len(got) != 0 {
		t.Errorf("first apply queued %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.paths.DropIn, "backend.conf")); !os.IsNotExist(err) {
		t.Errorf("first apply wrote a drop-in: %v", err)
	}
}

func TestPutModelTuning(t *testing.T) {
	f := newFixture(t)
	body := `{"ctx_size":262144,"sampling":{"temperature":0.6,"top_k":20},"speculative":{"types":["draft-simple"],"draft_model":"chat-14b","draft_max":3},"kv_cache":{"type_k":"q8_0","type_v":"q8_0"}}`
	if code, b := f.do(t, "PUT", "/models/big", body, true); code != http.StatusOK {
		t.Fatalf("PUT tuning = %d %v", code, b)
	}
	cfg, _ := os.ReadFile(filepath.Join(f.paths.Config, "llama-swap", "config.yaml"))
	if !strings.Contains(string(cfg), "--spec-draft-model /models/chat-14b.gguf") || !strings.Contains(string(cfg), "--temp 0.6") {
		t.Errorf("config:\n%s", cfg)
	}
	missing := `{"speculative":{"types":["draft-simple"],"draft_model":"nope"}}`
	if code, _ := f.do(t, "PUT", "/models/big", missing, true); code != http.StatusUnprocessableEntity {
		t.Errorf("missing draft model: code = %d, want 422", code)
	}
	self := `{"speculative":{"types":["draft-simple"],"draft_model":"big"}}`
	if code, _ := f.do(t, "PUT", "/models/big", self, true); code != http.StatusUnprocessableEntity {
		t.Errorf("self draft: code = %d, want 422", code)
	}
}

func TestBenchmark(t *testing.T) {
	f := newFixture(t)
	f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[1,0],"layout":"span"}`, true)
	if code, b := f.do(t, "POST", "/models/big/benchmark", "", true); code != http.StatusAccepted {
		t.Fatalf("start = %d %v", code, b)
	}
	dir := filepath.Join(f.run, "bench")
	for ext, want := range map[string]string{".path": "big-00001-of-00002.gguf", ".gpus": "0,1"} {
		got, _ := os.ReadFile(filepath.Join(dir, "big"+ext))
		if string(got) != want {
			t.Errorf("%s = %q, want %q", ext, got, want)
		}
	}
	if got := f.requests(t); !slices.Contains(got, "bench.big") {
		t.Errorf("requests = %v, want bench.big among them", got)
	}
	if code, b := f.do(t, "GET", "/models/big/benchmark", "", true); code != 200 || b["status"] != "queued" {
		t.Errorf("queued = %d %v", code, b)
	}

	// The agent finishes: HIP produced results, Vulkan failed.
	write(t, filepath.Join(dir, "big.status"), "done 2026-10-26T00:00:00Z")
	write(t, filepath.Join(dir, "big.hip.json"), `[{"n_prompt":512,"n_gen":0,"avg_ts":901.5},{"n_prompt":0,"n_gen":128,"avg_ts":38.2}]`)
	write(t, filepath.Join(dir, "big.vulkan.json"), ``)
	write(t, filepath.Join(dir, "big.vulkan.log"), "loading\nerror: failed to load model\n")
	code, b := f.do(t, "GET", "/models/big/benchmark", "", true)
	if code != 200 || b["recommended"] != "hip" {
		t.Fatalf("done = %d %v", code, b)
	}
	res := b["results"].(map[string]any)
	if res["hip"].(map[string]any)["generation_tokens_per_second"] != 38.2 {
		t.Errorf("hip = %v", res["hip"])
	}
	if res["vulkan"].(map[string]any)["error"] != "error: failed to load model" {
		t.Errorf("vulkan = %v", res["vulkan"])
	}
	if code, _ := f.do(t, "POST", "/models/missing/benchmark", "", true); code != http.StatusNotFound {
		t.Errorf("missing model = %d", code)
	}
}

func TestOpenWebUIIntegration(t *testing.T) {
	f := newFixture(t)
	var gotModel string
	owuiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/retrieval/embedding/update" {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotModel, _ = b["RAG_EMBEDDING_MODEL"].(string)
		}
		_, _ = w.Write([]byte(`{"RAG_EMBEDDING_ENGINE":"","RAG_EMBEDDING_MODEL":"x","openai_config":{"url":""}}`))
	}))
	defer owuiSrv.Close()
	f.server.OpenWebUI = &OpenWebUI{Base: owuiSrv.URL, EmbedURL: "http://llama-swap:8080/v1"}

	if code, _ := f.do(t, "PUT", "/integrations/open-webui", `{"api_key":"bad-key"}`, true); code != http.StatusUnprocessableEntity {
		t.Errorf("bad key = %d, want 422", code)
	}
	if _, err := os.Stat(filepath.Join(f.paths.Config, "open-webui.key")); !os.IsNotExist(err) {
		t.Error("a rejected key was stored")
	}
	f.do(t, "PUT", "/models/chat-14b", `{"embedding":true}`, true)
	code, b := f.do(t, "PUT", "/integrations/open-webui", `{"api_key":"good-key"}`, true)
	if code != 200 || b["configured"] != true || b["embedding_model"] != "chat-14b" {
		t.Fatalf("good key = %d %v", code, b)
	}
	if gotModel != "chat-14b" {
		t.Errorf("open webui was set to %q", gotModel)
	}
	if st, err := os.Stat(filepath.Join(f.paths.Config, "open-webui.key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", st, err)
	}
}
