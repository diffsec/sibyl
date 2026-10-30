package console

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		_, _ = io.WriteString(w, `[{"id":"qwen"},{"id":"gemma"},{"id":"embed","settings":{"embedding":true}}]`)
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
		Now: func() time.Time { return h.now },
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
