package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/arcane"
	"github.com/diffsec/sibyl/internal/ollama"
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

func TestRouterAnthropicProviderAndBudget(t *testing.T) {
	f := newFixture(t)
	var gotKey, gotVersion string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion = r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version")
		if r.URL.Path != "/v1/models" || gotKey != "good" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"first_id":null,"last_id":null}`))
	}))
	defer provider.Close()
	f.server.ProviderHTTP = provider.Client()

	body := func(key string) string {
		return fmt.Sprintf(`{"type":"anthropic","base_url":%q,"api_key":%q,"prices":{"claude-x":{"input":1,"output":2}}}`, provider.URL, key)
	}
	if code, b := f.do(t, "PUT", "/router/providers/claude", body("bad"), true); code != http.StatusUnprocessableEntity ||
		!strings.Contains(b["error"].(string), "refused the key") {
		t.Errorf("bad key = %d %v", code, b)
	}
	if code, b := f.do(t, "PUT", "/router/providers/claude", body("good"), true); code != 200 {
		t.Fatalf("good key = %d %v", code, b)
	}
	if gotVersion == "" {
		t.Error("the key check sent no anthropic-version")
	}

	if code, b := f.do(t, "POST", "/router/clients", `{"name":"laptop","policy":"hybrid","budget":{"daily_usd":2,"monthly_usd":20}}`, true); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, b)
	}
	if code, b := f.do(t, "PUT", "/router/clients/open-webui", `{"policy":"hybrid","budget":{"daily_usd":1}}`, true); code != 200 {
		t.Fatalf("open-webui budget = %d %v", code, b)
	}
	if code, _ := f.do(t, "PUT", "/router/clients/laptop", `{"policy":"hybrid","budget":{"daily_usd":-1}}`, true); code != http.StatusUnprocessableEntity {
		t.Errorf("negative budget = %d, want 422", code)
	}
	cfg, err := router.LoadConfig(filepath.Join(f.paths.Config, "router"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Providers["claude"].Prices["claude-x"].Output != 2 || cfg.Keys["claude"] != "good" {
		t.Errorf("providers %+v", cfg.Providers)
	}
	budgets := map[string]float64{}
	for _, c := range cfg.Clients.Clients {
		if c.Budget != nil {
			budgets[c.Name] = c.Budget.DailyUSD
		}
	}
	if budgets["laptop"] != 2 || budgets["open-webui"] != 1 {
		t.Errorf("budgets %v", budgets)
	}

	logDir := filepath.Join(f.paths.Config, "router", "log")
	sp := &router.Spend{Path: filepath.Join(logDir, "usage.json")}
	mkdir(t, logDir)
	if err := sp.Add("laptop", 0.75, time.Now()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(logDir, "decisions.jsonl"), `{"client":"laptop","route":"x/y","status":200,"unpriced":true}`+"\n")
	code, b := f.do(t, "GET", "/router/stats", "", true)
	spend, _ := b["spend"].(map[string]any)
	laptop, _ := spend["laptop"].(map[string]any)
	if code != 200 || laptop["today_usd"] != 0.75 || laptop["month_usd"] != 0.75 || b["unpriced"] != float64(1) {
		t.Errorf("stats = %d %v", code, b)
	}
}

func TestArcaneDefaultPasswordReplaced(t *testing.T) {
	f := newFixture(t)
	password := "arcane-admin"
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		switch r.URL.Path {
		case "/api/auth/login":
			logins++
			if b["password"] != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"token":"t"}}`))
		case "/api/auth/password":
			password = b["newPassword"]
			_, _ = w.Write([]byte(`{"data":{"message":"ok"}}`))
		}
	}))
	defer srv.Close()
	f.server.Arcane = &Arcane{Client: arcane.New(srv.URL)}
	f.server.SecureArcaneAtStart(t.Context(), time.Millisecond, 3)
	if password == "arcane-admin" || len(password) < 16 {
		t.Fatalf("password %q", password)
	}
	code, b := f.do(t, "GET", "/integrations/arcane", "", true)
	if code != 200 || b["password"] != password || b["user"] != "arcane" {
		t.Fatalf("status = %d %v", code, b)
	}
	// A second start finds the default refused and changes nothing.
	before := password
	f.server.SecureArcaneAtStart(t.Context(), time.Millisecond, 3)
	if password != before {
		t.Error("the password changed again")
	}
	// The owner changes the password in Arcane: the stored one goes away.
	password = "owner-chose-this"
	f.server.Arcane.checked = time.Time{}
	if _, b := f.do(t, "GET", "/integrations/arcane", "", true); b["password"] != nil {
		t.Errorf("stale password shown: %v", b)
	}
	if _, err := os.Stat(filepath.Join(f.paths.Config, "arcane-password")); !os.IsNotExist(err) {
		t.Error("the password file was kept")
	}
	// Polls within a minute do not sign in again.
	n := logins
	write(t, filepath.Join(f.paths.Config, "arcane-password"), "owner-chose-this")
	f.server.Arcane.checked = time.Time{}
	f.do(t, "GET", "/integrations/arcane", "", true)
	f.do(t, "GET", "/integrations/arcane", "", true)
	if logins != n+1 {
		t.Errorf("%d logins for two polls, want 1", logins-n)
	}
}

// TestOllamaTakesClientKeys checks that /api takes router client keys, not
// the System API key, and that a good key is kept for a minute.
func TestOllamaTakesClientKeys(t *testing.T) {
	f := newFixture(t)
	checks := 0
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		if r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer client-key" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rt.Close()
	f.server.Ollama = &ollama.Shim{Upstream: rt.URL, HTTP: &http.Client{},
		Models:    func(context.Context) ([]ollama.ModelInfo, error) { return []ollama.ModelInfo{{ID: "m"}}, nil },
		Authorize: (&RouterAuth{URL: rt.URL}).Authorize}
	srv := httptest.NewServer(f.server.Handler())
	defer srv.Close()
	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/tags", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for key, want := range map[string]int{"": 401, token: 401, "wrong": 401, "client-key": 200} {
		if got := get(key); got != want {
			t.Errorf("key %q: %d, want %d", key, got, want)
		}
	}
	n := checks
	get("client-key")
	if checks != n {
		t.Error("a good key was checked again within a minute")
	}
	// The client key does not open the System API.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("client key on /status: %v %v", resp.StatusCode, err)
	}
}

func TestRouterClientNewKey(t *testing.T) {
	f := newFixture(t)
	_, b := f.do(t, "POST", "/router/clients", `{"name":"laptop"}`, true)
	old := b["key"].(string)
	code, b := f.do(t, "POST", "/router/clients/laptop/key", "", true)
	key, _ := b["key"].(string)
	if code != 200 || key == "" || key == old {
		t.Fatalf("new key = %d %v", code, b)
	}
	cfg, err := router.LoadConfig(filepath.Join(f.paths.Config, "router"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if cfg.Clients.Clients[0].KeySHA256 != hex.EncodeToString(sum[:]) {
		t.Error("clients.yaml holds the old hash")
	}
	if code, _ := f.do(t, "POST", "/router/clients/default/key", "", true); code != http.StatusConflict {
		t.Errorf("built-in client = %d, want 409", code)
	}
	if code, _ := f.do(t, "POST", "/router/clients/nobody/key", "", true); code != http.StatusNotFound {
		t.Errorf("unknown client = %d, want 404", code)
	}
}

func TestCatalogFetch(t *testing.T) {
	f := newFixture(t)
	var fetches atomic.Int32
	var broken atomic.Bool
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		if broken.Load() {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write([]byte(`{"orcarouter":{"id":"orcarouter","name":"OrcaRouter","npm":"@ai-sdk/openai-compatible",
			"api":"https://api.orcarouter.ai/v1","env":["ORCAROUTER_API_KEY"],"models":{
			"deepseek/a":{"id":"deepseek/a","name":"A","cost":{"input":1,"output":2}},
			"qwen/b":{"id":"qwen/b","name":"B"}}}}`))
	}))
	defer src.Close()
	f.server.Catalog = &Catalog{URL: src.URL}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { f.server.KeepCatalog(ctx); close(done) }()
	for i := 0; i < 200 && fetches.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	code, b := f.do(t, "GET", "/router/catalog", "", true)
	ps, _ := b["providers"].([]any)
	if code != 200 || len(ps) != 1 || ps[0].(map[string]any)["base_url"] != "https://api.orcarouter.ai/v1" || b["fetched"] == nil {
		t.Fatalf("catalog = %d %v", code, b)
	}
	code, b = f.do(t, "GET", "/router/catalog/orcarouter?q=deepseek", "", true)
	if ms, _ := b["models"].([]any); code != 200 || len(ms) != 1 {
		t.Errorf("filtered models = %d %v", code, b)
	}
	if code, _ := f.do(t, "GET", "/router/catalog/nobody", "", true); code != 404 {
		t.Errorf("unknown provider = %d", code)
	}

	// A fresh copy is not fetched again at the next start.
	n := fetches.Load()
	ctx, cancel = context.WithCancel(t.Context())
	done = make(chan struct{})
	go func() { f.server.KeepCatalog(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if fetches.Load() != n {
		t.Error("a fresh copy was fetched again")
	}

	// The router reads the copy for prices.
	cfg, err := router.LoadConfig(filepath.Join(f.paths.Config, "router"))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := cfg.Catalog.Model("orcarouter", "deepseek/a"); !ok || m.Cost.Output != 2 {
		t.Errorf("router catalog %+v", m)
	}

	// A failed refresh keeps the copy and reports the error.
	broken.Store(true)
	defer broken.Store(false)
	if code, _ := f.do(t, "POST", "/router/catalog/refresh", "", true); code != http.StatusBadGateway {
		t.Errorf("failed refresh = %d", code)
	}
	if _, b := f.do(t, "GET", "/router/catalog", "", true); b["error"] == nil || len(b["providers"].([]any)) != 1 {
		t.Errorf("after a failed refresh %v", b)
	}
}

// TestCatalogShrinkRefused keeps the copy when a download lost most of
// its providers.
func TestCatalogShrinkRefused(t *testing.T) {
	f := newFixture(t)
	big := `{"a":{"npm":"@ai-sdk/openai-compatible","api":"https://a/v1","models":{}},
		"b":{"npm":"@ai-sdk/openai-compatible","api":"https://b/v1","models":{}},
		"c":{"npm":"@ai-sdk/openai-compatible","api":"https://c/v1","models":{}},
		"d":{"npm":"@ai-sdk/openai-compatible","api":"https://d/v1","models":{}}}`
	body := big
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer src.Close()
	f.server.Catalog = &Catalog{URL: src.URL}
	if code, b := f.do(t, "POST", "/router/catalog/refresh", "", true); code != 200 {
		t.Fatalf("first refresh = %d %v", code, b)
	}
	body = `{"a":{"npm":"@ai-sdk/openai-compatible","api":"https://a/v1","models":{}}}`
	code, b := f.do(t, "POST", "/router/catalog/refresh", "", true)
	if code != http.StatusBadGateway || !strings.Contains(b["error"].(string), "kept the copy") {
		t.Errorf("shrunk refresh = %d %v", code, b)
	}
	if _, b := f.do(t, "GET", "/router/catalog", "", true); len(b["providers"].([]any)) != 4 {
		t.Errorf("copy changed: %v", b["providers"])
	}
}
