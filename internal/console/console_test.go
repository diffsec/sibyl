package console

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/router"
)

const testHost = "sibyl.test:8443"

// fakeAPI is a System API that answers the routes of the console and
// records each change.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string]map[string]any
	profiles router.ProfilesFile
	auth     []string
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{bodies: map[string]map[string]any{}}
	yes := true
	f.profiles = router.ProfilesFile{Profiles: map[string]router.Profile{"auto": {LocalModel: "qwen", Rules: []router.Rule{
		{Name: "secrets", When: router.When{Sensitive: &yes}, Route: "local"},
		{Name: "schema", When: router.When{JSONSchema: &yes}, Route: "cloud/big"},
	}}}}
	return f
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if key == "PUT /router/profiles" {
			f.profiles = router.ProfilesFile{}
			_ = json.Unmarshal(b, &f.profiles)
		}
	}
	f.bodies[key] = body
	w.Header().Set("Content-Type", "application/json")
	switch key {
	case "GET /status":
		_, _ = io.WriteString(w, `{"version":"sha-test","image_tag":"sha-test-amd","variant":"amd","gpus":2,
			"hardware":{"backend":"hip","gpus":[0,1]},"services":{"updated":"now","units":{"router.service":{"active":"active","sub":"running"}}}}`)
	case "GET /router":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"clients": []map[string]any{{"name": "default", "policy": "local-only", "builtin": true},
				{"name": "laptop", "policy": "hybrid", "calibrate": true}},
			"providers": []map[string]any{{"name": "cloud", "type": "openai", "base_url": "https://x/v1", "models": []string{"big"},
				"key_set": true, "prices": map[string]any{"big": map[string]float64{"input": 1, "output": 2}}}},
			"profiles": f.profiles,
		})
	case "GET /router/stats":
		_, _ = io.WriteString(w, `{"requests":3,"errors":0,"by_client":{"laptop":3},"spend":{"laptop":{"today_usd":0.5}},
			"regenerated":{"local/qwen":1},"last":[{"client":"laptop","route":"local/qwen","status":200,"rule":"secrets <b>"}]}`)
	case "GET /router/calibration":
		_, _ = io.WriteString(w, `{"samples":2,"judged":2,"daily_usd":1,"cells":[{"local_model":"qwen","task":"code","difficulty":"hard",
			"comparisons":2,"local_ok_rate":0.5}],"proposals":[{"id":"abc","profile":"auto","task":"code","difficulty":"hard",
			"current":"local/qwen","route":"cloud/big","comparisons":50,"local_ok_rate":0.4}]}`)
	case "GET /models":
		_, _ = io.WriteString(w, `[{"id":"qwen","size_bytes":32212254720,"shards":2,"settings":{"ctx_size":65536,"sampling":{"temperature":0.6}},
			"meta":{"architecture":"qwen3moe","context_length":262144,"expert_count":128,"reasoning":{"var":"reasoning_effort","values":["low","high","xhigh"]}},
			"fit":{"ctx":65536,"slots":2,"slot_ctx":32768,"fits":true},"benchmark":{"status":"done","results":{"hip":{"generation_tokens_per_second":40.5,"prompt_tokens_per_second":900}},"recommended":"hip"}},
			{"id":"gemma"},{"id":"embed","settings":{"embedding":true}}]`)
	case "GET /hardware":
		_, _ = io.WriteString(w, `{"backend":"hip","gpus":[0],"layout":"span"}`)
	case "GET /gpus":
		_, _ = io.WriteString(w, `[{"index":0,"pci_address":"0000:03:00.0","vendor":"AMD","vram_total_bytes":25753000000},
			{"index":1,"pci_address":"0000:07:00.0","vendor":"AMD","vram_total_bytes":25753000000}]`)
	case "GET /host":
		_, _ = io.WriteString(w, `{"hostname":"sibyl","interfaces":[{"name":"enp8s0","state":"connected","method":"dhcp","addresses":["192.168.1.26/24"]}],
			"users":[{"name":"admin","groups":["wheel"],"admin":true},{"name":"ops","groups":["sibyl-operators"],"managed":true,"keys":1}],
			"pending_network":{"interface":"enp8s0","expires":"soon"},
			"disks":[{"name":"sdb","path":"/dev/sdb","size":64000000000,"model":"USB","serial":"SER123"}],
			"update":{"image":"ghcr.io/diffsec/sibyl-os:latest-amd","booted":"sha-old-amd","available":"sha-new-amd","check":{"active":"inactive","result":"success"}}}`)
	case "POST /host/update/check", "POST /host/update/apply", "POST /host/reboot":
		_, _ = io.WriteString(w, `{"ok":true,"message":"started by hostd"}`)
	case "GET /downloads":
		_, _ = io.WriteString(w, `[{"id":"j1","repo":"org/model-GGUF","variant":"Q8_0","status":"downloading","bytes_done":5368709120,"bytes_total":10737418240},
			{"id":"j2","repo":"org/other-GGUF","variant":"Q4_K_M","status":"failed","error":"disk full"}]`)
	case "GET /hf/search":
		_, _ = io.WriteString(w, `[{"id":"org/model-GGUF","downloads":10,"likes":2}]`)
	case "GET /hf/repos/org/model-GGUF":
		_, _ = io.WriteString(w, `{"repo":"org/model-GGUF","revision":"abc123def4567890","vram_bytes":51506000000,"ram_bytes":68719476736,
			"variants":[{"name":"Q8_0","size_bytes":10737418240,"shards":1,"fit":"gpu"}]}`)
	case "GET /pools":
		_, _ = io.WriteString(w, `{"pools":[{"name":"pool-1","mounted":true}]}`)
	case "GET /integrations/open-webui":
		_, _ = io.WriteString(w, `{"configured":false}`)
	case "GET /integrations/huggingface":
		_, _ = io.WriteString(w, `{"configured":true,"user":"ben"}`)
	case "PUT /host/hostname", "PUT /host/network/enp8s0", "PUT /host/users/ops", "POST /host/network/confirm":
		_, _ = io.WriteString(w, `{"ok":true,"message":"done by hostd"}`)
	case "DELETE /host/users/ops":
		_, _ = io.WriteString(w, `{"ok":false,"error":"refused by hostd"}`)
	case "POST /router/clients":
		_, _ = io.WriteString(w, `{"name":"phone","policy":"hybrid","key":"sib-secret-key"}`)
	case "DELETE /router/calibration":
		_, _ = io.WriteString(w, `{"deleted":2}`)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func (f *fakeAPI) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

type harness struct {
	t    *testing.T
	api  *fakeAPI
	con  *Console
	h    http.Handler
	now  time.Time
	jar  string // the session cookie
	addr string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, api: newFakeAPI(), now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), addr: "192.168.1.50"}
	h.con = &Console{API: h.api, Token: "api-key", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return h.now },
		TrustProxy: func(ip string) bool { return ip == "192.0.2.1" },
		Check: func(_ context.Context, user, password string) error {
			if user == "admin" && password == "right" {
				return nil
			}
			return ErrDenied
		}}
	h.h = h.con.Handler()
	return h
}

// do sends a request as a browser on a console page would.
func (h *harness) do(method, path string, form url.Values, origin bool) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "https://"+testHost+path, body)
	r.Host = testHost
	r.Header.Set("X-Forwarded-For", h.addr)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin {
		r.Header.Set("Origin", "https://"+testHost)
	}
	if h.jar != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: h.jar})
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) login() {
	h.t.Helper()
	w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true)
	if w.Code != http.StatusSeeOther {
		h.t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			h.jar = c.Value
		}
	}
}

func TestSignInFlow(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/console/router", nil, false); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/console/login" {
		t.Fatalf("no session: %d %q", w.Code, w.Header().Get("Location"))
	}
	w := h.do("GET", "/console/login", nil, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="password"`) {
		t.Fatalf("login page %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp != CSP || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers %v", w.Header())
	}
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, false); w.Code != http.StatusForbidden {
		t.Errorf("sign-in from another origin = %d", w.Code)
	}
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"wrong"}}, true); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d", w.Code)
	}

	h.login()
	var ck *http.Cookie
	w = h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true)
	for _, c := range w.Result().Cookies() {
		ck = c
	}
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != Prefix {
		t.Errorf("cookie %+v", ck)
	}
	w = h.do("GET", "/console/router", nil, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "laptop") {
		t.Fatalf("router page %d", w.Code)
	}
	// The console calls the API with the API key.
	if a := h.api.auth[len(h.api.auth)-1]; a != "Bearer api-key" {
		t.Errorf("API auth %q", a)
	}

	// The session ends after an hour without a request.
	h.now = h.now.Add(SessionIdle + time.Minute)
	if w := h.do("GET", "/console/router", nil, false); w.Code != http.StatusSeeOther {
		t.Errorf("idle session = %d", w.Code)
	}
}

func TestSessionMaxAndLogout(t *testing.T) {
	h := newHarness(t)
	h.login()
	for i := 0; i < 13; i++ {
		h.now = h.now.Add(55 * time.Minute)
		w := h.do("GET", "/console/status", nil, false)
		if h.now.Sub(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)) > SessionMax {
			if w.Code != http.StatusSeeOther {
				t.Fatalf("a session older than %s is kept", SessionMax)
			}
			break
		}
		if w.Code != 200 {
			t.Fatalf("status %d at %s", w.Code, h.now)
		}
	}
	h.now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h.login()
	if w := h.do("POST", "/console/logout", url.Values{}, true); w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := h.do("GET", "/console/status", nil, false); w.Code != http.StatusSeeOther {
		t.Errorf("a signed-out session works: %d", w.Code)
	}
}

func TestSignInLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < failLimit; i++ {
		if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"no"}}, true); w.Code != http.StatusUnauthorized {
			t.Fatalf("try %d = %d", i, w.Code)
		}
	}
	// Even the right password waits now.
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures = %d", failLimit, w.Code)
	}
	// Another address is not limited, and a minute later neither is this one.
	h.addr = "192.168.1.51"
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true); w.Code != http.StatusSeeOther {
		t.Errorf("another address = %d", w.Code)
	}
	h.addr = "192.168.1.50"
	h.now = h.now.Add(time.Minute)
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true); w.Code != http.StatusSeeOther {
		t.Errorf("a minute later = %d", w.Code)
	}
}

func TestChangeNeedsOrigin(t *testing.T) {
	h := newHarness(t)
	h.login()
	w := h.do("POST", "/console/router/client/delete", url.Values{"name": {"laptop"}}, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("change without Origin = %d", w.Code)
	}
	for _, c := range h.api.calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("the API got %s", c)
		}
	}
}

func TestPagesRender(t *testing.T) {
	h := newHarness(t)
	h.login()
	for path, want := range map[string][]string{
		"/console/status":                         {"sha-test-amd", "router.service", "active (running)"},
		"/console/router":                         {"laptop", "calibrates", "cloud", "sensitive=true", "json_schema=true", "Proposal for auto", "asked again within a minute"},
		"/console/router/live":                    {"Last decisions", "50%"},
		"/console/router/profile?name=auto":       {"Profile auto", `value="gemma"`},
		"/console/router/profile":                 {"New profile"},
		"/console/router/rule?profile=auto&i=0":   {"Rule secrets", `value="true" selected`},
		"/console/router/rule?profile=auto&i=new": {"New rule"},
	} {
		w := h.do("GET", path, nil, false)
		if w.Code != 200 {
			t.Errorf("%s = %d %s", path, w.Code, w.Body)
			continue
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("%s lacks %q", path, s)
			}
		}
		// Template escaping: a rule name from the log cannot add markup.
		if strings.Contains(w.Body.String(), "secrets <b>") {
			t.Errorf("%s holds unescaped markup", path)
		}
	}
	if w := h.do("GET", "/console/static/htmx-2.0.4.min.js", nil, false); w.Code != 200 {
		t.Errorf("htmx = %d", w.Code)
	}
}

func TestNewClientKeyShownOnce(t *testing.T) {
	h := newHarness(t)
	h.login()
	w := h.do("POST", "/console/router/client", url.Values{"name": {"phone"}, "policy": {"hybrid"}, "daily_usd": {"2"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "sib-secret-key") {
		t.Fatalf("new client = %d", w.Code)
	}
	if b := h.api.body("POST /router/clients"); b["name"] != "phone" || b["budget"].(map[string]any)["daily_usd"] != 2.0 {
		t.Errorf("API body %v", b)
	}
	if w := h.do("GET", "/console/router", nil, false); strings.Contains(w.Body.String(), "sib-secret-key") {
		t.Error("the key shows again")
	}
}

func TestClientEdit(t *testing.T) {
	h := newHarness(t)
	h.login()
	w := h.do("POST", "/console/router/client/edit", url.Values{"name": {"laptop"}, "policy": {"hybrid"}, "calibrate": {"on"}}, true)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "done=") {
		t.Fatalf("edit = %d %q", w.Code, w.Header().Get("Location"))
	}
	if b := h.api.body("PUT /router/clients/laptop"); b["calibrate"] != true || b["allow_hint"] != false || b["budget"] != nil {
		t.Errorf("API body %v", b)
	}
}

func TestProviderKeepsPrices(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.do("POST", "/console/router/provider", url.Values{"name": {"cloud"}, "type": {"openai"}, "base_url": {"https://x/v1"}, "models": {"big small"}}, true)
	b := h.api.body("PUT /router/providers/cloud")
	if p, _ := b["prices"].(map[string]any); p["big"] == nil {
		t.Errorf("prices lost: %v", b)
	}
	if _, set := b["api_key"]; set {
		t.Errorf("an empty key was sent: %v", b)
	}
}

func TestRuleChanges(t *testing.T) {
	h := newHarness(t)
	h.login()
	// Edit the second rule: its json_schema condition, which the form does
	// not show, stays.
	h.do("POST", "/console/router/rule", url.Values{"profile": {"auto"}, "i": {"1"}, "rule_name": {"schema to cloud"},
		"route": {"cloud/big"}, "sensitive": {"false"}, "task": {"code", "math"}, "prompt_tokens_over": {"local_context"}}, true)
	r := h.api.profiles.Profiles["auto"].Rules[1]
	if r.Name != "schema to cloud" || r.When.JSONSchema == nil || r.When.Sensitive == nil || *r.When.Sensitive ||
		len(r.When.Task) != 2 || r.When.PromptTokensOver == nil || !r.When.PromptTokensOver.LocalContext {
		t.Fatalf("rule %+v", r)
	}
	h.do("POST", "/console/router/rule", url.Values{"profile": {"auto"}, "i": {"new"}, "rule_name": {"rest"}, "route": {"local"}}, true)
	h.do("POST", "/console/router/rule/move", url.Values{"profile": {"auto"}, "i": {"2"}, "dir": {"up"}}, true)
	rules := h.api.profiles.Profiles["auto"].Rules
	if len(rules) != 3 || rules[1].Name != "rest" {
		t.Fatalf("rules %+v", rules)
	}
	h.do("POST", "/console/router/rule/delete", url.Values{"profile": {"auto"}, "i": {"0"}}, true)
	if rules := h.api.profiles.Profiles["auto"].Rules; len(rules) != 2 || rules[0].Name != "rest" {
		t.Errorf("rules after delete %+v", rules)
	}
	w := h.do("POST", "/console/router/rule/move", url.Values{"profile": {"auto"}, "i": {"0"}, "dir": {"up"}}, true)
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Errorf("moving the first rule up gave %q", w.Header().Get("Location"))
	}
}

func TestProfileAndSettings(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.do("POST", "/console/router/profile", url.Values{"name": {"auto"}, "local_model": {"gemma"}, "fallback": {"local cloud/big"}}, true)
	p := h.api.profiles.Profiles["auto"]
	if p.LocalModel != "gemma" || len(p.Fallback) != 2 || len(p.Rules) != 2 {
		t.Fatalf("profile %+v", p)
	}
	w := h.do("POST", "/console/router/profile", url.Values{"new": {"1"}, "name": {"auto"}, "local_model": {"qwen"}}, true)
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Errorf("a new profile replaced auto: %q", w.Header().Get("Location"))
	}
	h.do("POST", "/console/router/calibration", url.Values{"sample_rate": {"20"}, "daily_usd": {"0.5"}, "judge": {"local"}, "auto_apply": {"on"}}, true)
	cc := h.api.profiles.Calibration
	if cc.SampleRate == nil || *cc.SampleRate != 0.2 || cc.Daily() != 0.5 || cc.Judge != "local" || !cc.AutoApply || !cc.NoSeed {
		t.Errorf("calibration %+v", cc)
	}
	h.do("POST", "/console/router/classifier", url.Values{"sensitive_words": {"project-x, acme corp"}, "starter": {"on"}, "k": {"7"}}, true)
	f := h.api.profiles
	if len(f.SensitiveWords) != 2 || f.Classifier.K != 7 || f.Classifier.NoStarter || f.Profiles["auto"].LocalModel != "gemma" {
		t.Errorf("classifier %+v %+v", f.Classifier, f.SensitiveWords)
	}
	h.do("POST", "/console/router/calibration/apply", url.Values{"id": {"abc"}}, true)
	if b := h.api.body("POST /router/calibration/apply"); b["id"] != "abc" {
		t.Errorf("apply body %v", b)
	}
	w = h.do("POST", "/console/router/calibration/delete", url.Values{}, true)
	if !strings.Contains(w.Header().Get("Location"), "delete+2+calibration+samples") {
		t.Errorf("delete samples %q", w.Header().Get("Location"))
	}
}

func TestForwardedForOnlyFromProxy(t *testing.T) {
	h := newHarness(t)
	h.con.TrustProxy = func(string) bool { return false }
	// Each request names another address, but none comes from the proxy,
	// so all count against the one connection address.
	for i := 0; i < failLimit; i++ {
		h.addr = "10.0.0." + strconv.Itoa(i)
		h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"no"}}, true)
	}
	h.addr = "10.0.0.99"
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"right"}}, true); w.Code != http.StatusTooManyRequests {
		t.Errorf("a forged X-Forwarded-For passed the limit: %d", w.Code)
	}
}

func TestPollDoesNotKeepSession(t *testing.T) {
	h := newHarness(t)
	h.login()
	poll := func() int {
		r := httptest.NewRequest("GET", "https://"+testHost+"/console/router/live", nil)
		r.Header.Set("HX-Request", "true")
		r.AddCookie(&http.Cookie{Name: CookieName, Value: h.jar})
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 11; i++ {
		h.now = h.now.Add(5 * time.Minute)
		if code := poll(); i < 11 && h.now.Sub(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)) <= SessionIdle && code != 200 {
			t.Fatalf("poll %d = %d", i, code)
		}
	}
	h.now = h.now.Add(10 * time.Minute)
	if code := poll(); code != http.StatusUnauthorized {
		t.Errorf("an hour of polls kept the session: %d", code)
	}
}

func TestFlashOnlySignedIn(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/console/login?done=Password+expired", nil, false); strings.Contains(w.Body.String(), "Password expired") {
		t.Error("the sign-in page shows text from the URL")
	}
	h.login()
	if w := h.do("GET", "/console/status?done=saved", nil, false); !strings.Contains(w.Body.String(), "saved") {
		t.Error("a signed-in page does not show the flash")
	}
}

func TestChecksRunTwoAtOnce(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	h.con.Check = func(context.Context, string, string) error {
		started <- struct{}{}
		<-release
		return ErrDenied
	}
	var wg sync.WaitGroup
	for i := 0; i < maxChecks; i++ {
		wg.Add(1)
		h2 := *h
		h2.addr = "192.168.1." + strconv.Itoa(60+i)
		go func() {
			defer wg.Done()
			h2.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"x"}}, true)
		}()
	}
	for i := 0; i < maxChecks; i++ {
		<-started
	}
	h.addr = "192.168.1.70"
	if w := h.do("POST", "/console/login", url.Values{"user": {"admin"}, "password": {"x"}}, true); w.Code != http.StatusTooManyRequests {
		t.Errorf("a third check at once = %d", w.Code)
	}
	close(release)
	wg.Wait()
}

func TestOriginNullUsesFetchSite(t *testing.T) {
	h := newHarness(t)
	h.login()
	post := func(origin, site string) int {
		r := httptest.NewRequest("POST", "https://"+testHost+"/console/router/client/delete", strings.NewReader("name=laptop"))
		r.Host = testHost
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		r.AddCookie(&http.Cookie{Name: CookieName, Value: h.jar})
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		origin, site string
		want         int
	}{
		{"null", "same-origin", http.StatusSeeOther},
		{"", "same-origin", http.StatusSeeOther},
		{"null", "cross-site", http.StatusForbidden},
		{"null", "", http.StatusForbidden},
		{"https://evil.example", "same-origin", http.StatusForbidden},
	} {
		if got := post(c.origin, c.site); got != c.want {
			t.Errorf("Origin %q Sec-Fetch-Site %q = %d, want %d", c.origin, c.site, got, c.want)
		}
	}
	if w := h.do("GET", "/console/login", nil, false); w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Errorf("Referrer-Policy %q", w.Header().Get("Referrer-Policy"))
	}
}

func TestMorePagesRender(t *testing.T) {
	h := newHarness(t)
	h.login()
	for path, want := range map[string][]string{
		"/console/models":                           {"qwen", "30.0 GiB", "context 65536 in 2 slots of 32768", "hip 40.5 tok/s"},
		"/console/models/edit?id=qwen":              {"qwen3moe", "native context 262144", `name="moelayers"`, `value="0.6"`, `value="65536"`, `<option value="reasoning_effort=xhigh">xhigh</option>`, "reads reasoning_effort"},
		"/console/downloads":                        {"org/model-GGUF", "downloading", "5.0 GiB of 10.0 GiB (50%)", "disk full", "Retry", "Set for ben"},
		"/console/downloads?q=qwen":                 {`href="/console/downloads/repo?id=org%2fmodel-GGUF"`},
		"/console/downloads/repo?id=org/model-GGUF": {"Q8_0", "fits in VRAM", "pool-1", "abc123def4567890"},
		"/console/hardware":                         {"GPU 0", "GPU 1", `value="0" checked`, "hip"},
		"/console/host":                             {"enp8s0", "192.168.1.26/24", "ops", "Type SER123 to confirm", "Keep the network change"},
		"/console/status":                           {"router.service", "Restart router.service", "Not connected"},
		"/console/setup":                            {"GPUs, backend and layout", "done", "to do"},
	} {
		w := h.do("GET", path, nil, false)
		if w.Code != 200 {
			t.Errorf("%s = %d %s", path, w.Code, w.Body)
			continue
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("%s lacks %q", path, s)
			}
		}
	}
	// An exact owner/name opens the repository without a search.
	if w := h.do("GET", "/console/downloads?q=org/model-GGUF", nil, false); w.Code != http.StatusSeeOther ||
		!strings.Contains(w.Header().Get("Location"), "/console/downloads/repo?id=org%2Fmodel-GGUF") {
		t.Errorf("exact repo = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestModelAndHostChanges(t *testing.T) {
	h := newHarness(t)
	h.login()
	w := h.do("POST", "/console/models/edit", url.Values{"id": {"qwen"}, "ctxsize": {"131072"}, "temperature": {"0.7"},
		"spectypes": {"ngram-mod"}, "moelayers": {"all"}, "embedding": {"on"}, "reasoningvar": {"reasoning_effort"}, "reasoning": {"xhigh"}}, true)
	if !strings.Contains(w.Header().Get("Location"), "done=saved+qwen") {
		t.Fatalf("save model %q", w.Header().Get("Location"))
	}
	b := h.api.body("PUT /models/qwen")
	if b["ctx_size"] != 131072.0 || b["embedding"] != true || b["sampling"].(map[string]any)["temperature"] != 0.7 ||
		b["moe"].(map[string]any)["cpu_layers"] == nil || b["reasoning"].(map[string]any)["value"] != "xhigh" {
		t.Errorf("settings %v", b)
	}
	w = h.do("POST", "/console/models/edit", url.Values{"id": {"qwen"}, "temperature": {"warm"}}, true)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "models/edit?id=qwen&error=") {
		t.Errorf("bad value %q", loc)
	}

	h.do("POST", "/console/hardware", url.Values{"gpus": {"0", "1"}, "backend": {"vulkan"}, "layout": {"pin"}}, true)
	if b := h.api.body("PUT /hardware"); b["backend"] != "vulkan" || len(b["gpus"].([]any)) != 2 || b["layout"] != "pin" {
		t.Errorf("hardware %v", b)
	}
	if w := h.do("POST", "/console/hardware", url.Values{"backend": {"hip"}}, true); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Error("hardware with no GPU passed")
	}

	w = h.do("POST", "/console/host/network", url.Values{"interface": {"enp8s0"}, "method": {"static"}, "address": {"192.168.1.30/24"},
		"gateway": {"192.168.1.1"}, "dns": {"1.1.1.1 9.9.9.9"}}, true)
	if b := h.api.body("PUT /host/network/enp8s0"); b["address"] != "192.168.1.30/24" || len(b["dns"].([]any)) != 2 {
		t.Errorf("network %v", b)
	}
	if !strings.Contains(w.Header().Get("Location"), "done+by+hostd") {
		t.Errorf("network result %q", w.Header().Get("Location"))
	}
	w = h.do("POST", "/console/host/user/delete", url.Values{"name": {"ops"}}, true)
	if !strings.Contains(w.Header().Get("Location"), "error=refused+by+hostd") {
		t.Errorf("a refusal from hostd is lost: %q", w.Header().Get("Location"))
	}
	if w := h.do("POST", "/console/host/user", url.Values{"name": {"ops"}}, true); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Error("an operator with no key passed")
	}

	h.do("POST", "/console/downloads", url.Values{"repo": {"org/model-GGUF"}, "revision": {"abc"}, "variant": {"Q8_0"}, "pool": {"pool-1"}}, true)
	if b := h.api.body("POST /downloads"); b["repo"] != "org/model-GGUF" || b["variant"] != "Q8_0" || b["pool"] != "pool-1" {
		t.Errorf("download %v", b)
	}
	h.do("POST", "/console/status/openwebui", url.Values{"mode": {"create"}, "name": {"Ben"}, "email": {"b@x"}, "password": {"pw"}}, true)
	if b := h.api.body("POST /integrations/open-webui/connect"); b["create_admin"] != true || b["password"] != "pw" {
		t.Errorf("connect %v", b)
	}
}

func TestUpdateControls(t *testing.T) {
	h := newHarness(t)
	h.login()
	w := h.do("GET", "/console/status", nil, false)
	for _, want := range []string{"sha-old-amd", "Apply update sha-new-amd", "Check for an update"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("status lacks %q", want)
		}
	}
	w = h.do("POST", "/console/status/update/apply", url.Values{"reboot": {"on"}}, true)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/console/status?done=update") || !strings.Contains(loc, "started+by+hostd") {
		t.Errorf("apply %q", loc)
	}
	if b := h.api.body("POST /host/update/apply"); b["reboot"] != true {
		t.Errorf("apply body %v", b)
	}
	h.do("POST", "/console/status/update/apply", url.Values{}, true)
	if b := h.api.body("POST /host/update/apply"); b["reboot"] != false {
		t.Errorf("apply without reboot %v", b)
	}
	h.do("POST", "/console/status/update/check", url.Values{}, true)
	if !slices.Contains(h.api.calls, "POST /host/update/check") {
		t.Error("no check call")
	}
}
