package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/router"
)

// TestRouterConfig checks that what the System API writes is a
// configuration that the router itself loads.
func TestRouterConfig(t *testing.T) {
	f := newFixture(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer provider.Close()
	f.server.ProviderHTTP = provider.Client()
	dir := filepath.Join(f.paths.Config, "router")

	// Clients.
	code, b := f.do(t, "POST", "/router/clients", `{"name":"laptop","policy":"hybrid"}`, true)
	key, _ := b["key"].(string)
	if code != http.StatusCreated || !strings.HasPrefix(key, "sib-") {
		t.Fatalf("create client = %d %v", code, b)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, router.ClientsName))
	sum := sha256.Sum256([]byte(key))
	if strings.Contains(string(raw), key) || !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Errorf("clients.yaml must hold the hash, not the key:\n%s", raw)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/router/clients", `{"name":"laptop"}`},
		{"POST", "/router/clients", `{"name":"default"}`},
		{"POST", "/router/clients", `{"name":"Bad Name"}`},
		{"POST", "/router/clients", `{"name":"x","policy":"open"}`},
		{"DELETE", "/router/clients/default", ``},
		{"PUT", "/router/clients/nobody", `{"policy":"hybrid"}`},
	} {
		if code, b := f.do(t, c.method, c.path, c.body, true); code < 400 {
			t.Errorf("%s %s %s = %d %v, want an error", c.method, c.path, c.body, code, b)
		}
	}
	if code, b := f.do(t, "PUT", "/router/clients/open-webui", `{"policy":"hybrid"}`, true); code != 200 {
		t.Errorf("set open-webui policy = %d %v", code, b)
	}

	// Providers.
	put := func(body string) (int, map[string]any) {
		return f.do(t, "PUT", "/router/providers/cloud", body, true)
	}
	base := provider.URL + "/v1"
	if code, _ := put(fmt.Sprintf(`{"type":"openai","base_url":%q,"api_key":"bad"}`, base)); code != http.StatusUnprocessableEntity {
		t.Errorf("bad key = %d, want 422", code)
	}
	if code, _ := put(fmt.Sprintf(`{"type":"openai","base_url":%q}`, base)); code != http.StatusUnprocessableEntity {
		t.Errorf("no key = %d, want 422", code)
	}
	if code, b := put(fmt.Sprintf(`{"type":"openai","base_url":%q,"api_key":"good","models":["big"]}`, base)); code != 200 {
		t.Fatalf("good key = %d %v", code, b)
	}
	if st, err := os.Stat(filepath.Join(dir, "keys", "cloud")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file %v %v", st, err)
	}
	// A change without a key keeps the stored key.
	if code, b := put(fmt.Sprintf(`{"type":"openai","base_url":%q,"models":["big","small"]}`, base)); code != 200 {
		t.Errorf("change without key = %d %v", code, b)
	}

	// Profiles.
	profiles := `{"profiles":{"auto":{"local_model":"chat-14b","rules":[
		{"name":"long","when":{"prompt_tokens_over":"local_context"},"route":"cloud/big"},
		{"name":"rest","route":"local"}],"fallback":["local"]}}}`
	if code, b := f.do(t, "PUT", "/router/profiles", profiles, true); code != 200 {
		t.Fatalf("profiles = %d %v", code, b)
	}
	if code, _ := f.do(t, "PUT", "/router/profiles", `{"profiles":{"auto":{"local_model":"m","rules":[{"name":"x","route":"nowhere/y"}]}}}`, true); code != http.StatusUnprocessableEntity {
		t.Errorf("bad profile = %d, want 422", code)
	}
	if code, _ := f.do(t, "DELETE", "/router/providers/cloud", "", true); code != http.StatusConflict {
		t.Errorf("delete a provider that a profile uses = %d, want 409", code)
	}

	// The router loads what the System API wrote.
	if code, _ := f.do(t, "POST", "/apply", "", true); code != 200 {
		t.Fatal("apply failed")
	}
	cfg, err := router.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients.Clients) != 2 || cfg.Keys["cloud"] != "good" || len(cfg.Providers.Providers["cloud"].Models) != 2 ||
		!cfg.Profiles.Profiles["auto"].Rules[0].When.PromptTokensOver.LocalContext {
		t.Errorf("router config %+v", cfg)
	}
	var ids []string
	for _, m := range cfg.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "big,chat-14b" {
		t.Errorf("models.json ids %v", ids)
	}

	code, b = f.do(t, "GET", "/router", "", true)
	clients, _ := b["clients"].([]any)
	if code != 200 || len(clients) != 3 {
		t.Fatalf("GET /router = %d %v", code, b)
	}
	if c := clients[1].(map[string]any); c["name"] != "open-webui" || c["policy"] != "hybrid" || c["builtin"] != true {
		t.Errorf("open-webui view %v", c)
	}
	if p := b["providers"].([]any)[0].(map[string]any); p["key_set"] != true || p["api_key"] != nil {
		t.Errorf("provider view %v", p)
	}
	if code, _ := f.do(t, "DELETE", "/router/clients/laptop", "", true); code != 200 {
		t.Error("delete client failed")
	}
}

func TestRouterStatsAndExplain(t *testing.T) {
	f := newFixture(t)
	logDir := filepath.Join(f.paths.Config, "router", "log")
	mkdir(t, logDir)
	var lines []string
	for i, e := range []router.Entry{
		{Client: "default", Route: "local/a", Status: 200},
		{Client: "laptop", Route: "cloud/big", Status: 200},
		{Client: "laptop", Route: "", Status: 403, Error: "must stay"},
	} {
		e.LatencyMS = int64(i)
		b, _ := json.Marshal(e)
		lines = append(lines, string(b))
	}
	write(t, filepath.Join(logDir, "decisions.jsonl"), strings.Join(lines, "\n")+"\nnot json\n")
	code, b := f.do(t, "GET", "/router/stats", "", true)
	if code != 200 || b["requests"] != float64(3) || b["errors"] != float64(1) ||
		b["by_client"].(map[string]any)["laptop"] != float64(2) {
		t.Errorf("stats = %d %v", code, b)
	}
	if last := b["last"].([]any); last[0].(map[string]any)["status"] != float64(403) {
		t.Errorf("the newest entry is not first: %v", last)
	}

	var gotAuth string
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"route":"local/a"}`))
	}))
	defer rt.Close()
	f.server.RouterURL = rt.URL
	code, b = f.do(t, "POST", "/router/explain", `{"client":"default","request":{"model":"auto"}}`, true)
	if code != 200 || b["route"] != "local/a" || gotAuth != "Bearer "+token {
		t.Errorf("explain = %d %v auth %q", code, b, gotAuth)
	}
}

func TestSyncOpenWebUIAtStartRetries(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.paths.Config, "open-webui.key"), "good-key")
	calls := 0
	owuiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ENABLE_OPENAI_API":true,"OPENAI_API_BASE_URLS":["http://router:8080/v1"],"OPENAI_API_KEYS":["k"],"OPENAI_API_CONFIGS":{}}`))
	}))
	defer owuiSrv.Close()
	f.server.OpenWebUI = &OpenWebUI{Base: owuiSrv.URL, URL: "http://router:8080/v1", Key: "k"}
	f.server.SyncOpenWebUIAtStart(t.Context(), time.Millisecond, 5)
	if calls != 2 {
		t.Errorf("Open WebUI got %d calls, want 2: one failure, then one sync", calls)
	}
	if f.server.OpenWebUI.lastErr != "" {
		t.Errorf("last error %q", f.server.OpenWebUI.lastErr)
	}
}
