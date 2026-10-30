package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/router"
)

func routerFixture() routerLoaded {
	var r client.Router
	r.Clients = []client.RouterClient{
		{Name: "default", Policy: "local-only", Builtin: true},
		{Name: "open-webui", Policy: "hybrid", Builtin: true},
		{Name: "laptop", Policy: "hybrid", Budget: &client.RouterBudget{DailyUSD: 2}},
	}
	r.Providers = []client.RouterProvider{{Name: "orcarouter", Type: "openai", BaseURL: "https://api.orcarouter.ai/v1",
		Models: []string{"deepseek/deepseek-v4.1-flash-1017"}, KeySet: true}}
	yes := true
	r.Profiles.Profiles = map[string]router.Profile{"auto": {LocalModel: "qwen", Rules: []router.Rule{
		{Name: "secrets", When: router.When{Sensitive: &yes}, Route: "local"},
		{Name: "long", When: router.When{PromptTokensOver: &router.Tokens{LocalContext: true}}, Route: "orcarouter/deepseek/deepseek-v4.1-flash-1017"}}}}
	rate := 0.2
	r.Profiles.Calibration.SampleRate = &rate
	st := client.RouterStats{Requests: 12, Errors: 1, Unpriced: 3,
		ByClient: map[string]int{"open-webui": 7, "laptop": 5},
		Spend:    map[string]client.RouterSpend{"laptop": {Today: 1.25, Month: 4}, "open-webui": {Today: 0.5, Month: 0.5}},
		Last: []client.RouterEntry{{Time: time.Now(), Client: "laptop", Route: "orcarouter/deepseek/deepseek-v4.1-flash-1017",
			Rule: "long", Status: 200, CostUSD: 0.0123}}}
	return routerLoaded{r: r, st: st}
}

func TestRouterRender(t *testing.T) {
	v := newRouterView(nil)
	v.update(routerFixture())
	out := ansi.ReplaceAllString(v.render(120), "")
	for _, want := range []string{
		"default", "stays on this host",
		"open-webui", "$0.50 today, $0.50 this month  no budget",
		"laptop", "$1.25 today of $2.00, $4.00 this month",
		"orcarouter", "key set", "deepseek/deepseek-v4.1-flash-1017",
		"auto           local qwen",
		"1. secrets", "sensitive=true → local",
		"2. long", "prompt>local_context → orcarouter/deepseek/deepseek-v4.1-flash-1017",
		"12 requests, 1 errors, 3 with no price", "$0.0123  long",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	// Built-in clients cannot be deleted.
	if v.keyPress("d") != nil || v.form != nil {
		t.Error("d opened a form for a built-in client")
	}
}

// TestRouterNewClientShowsKey runs the new client form against a fake API.
func TestRouterNewClientShowsKey(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/router/clients" {
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"name":"laptop","policy":"hybrid","key":"sib-newkey"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newRouterView(c)
	v.update(routerFixture())
	v.keyPress("n")
	if v.form == nil || !v.capturing() {
		t.Fatal("n did not open the form")
	}
	// Fill the form's values and finish it, as enter on the last field does.
	v.name, v.policy, v.daily, v.monthly = "laptop", "hybrid", "3.5", ""
	v.form = nil
	cmd := v.submit()
	// Run the API call from the batch, and feed its result back.
	for _, m := range drain(cmd) {
		v.update(m)
	}
	if got["name"] != "laptop" || got["policy"] != "hybrid" || got["budget"].(map[string]any)["daily_usd"] != 3.5 {
		t.Errorf("request %v", got)
	}
	out := ansi.ReplaceAllString(v.render(120), "")
	if !strings.Contains(out, "Key for laptop") || !strings.Contains(out, "sib-newkey") {
		t.Errorf("key not shown:\n%s", out)
	}
}

// drain runs a command and the commands of a batch, and returns their
// messages, skipping ticks.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-ctx.Done():
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drain(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestRouterBudgetParse(t *testing.T) {
	v := &routerView{daily: " ", monthly: ""}
	if v.budget() != nil {
		t.Error("empty fields gave a budget")
	}
	v.daily, v.monthly = "", "10"
	if b := v.budget(); b == nil || b.MonthlyUSD != 10 || b.DailyUSD != 0 {
		t.Errorf("budget %+v", b)
	}
	for _, s := range []string{"-1", "five"} {
		if validUSD(s) == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestProviderPicker(t *testing.T) {
	v := newRouterView(nil)
	v.update(routerFixture())
	cmd := v.update(catalogLoaded{cat: client.Catalog{Fetched: "2026-10-29T03:00:00Z", Providers: []client.CatalogProvider{
		{ID: "orcarouter", Name: "OrcaRouter", Type: "openai", BaseURL: "https://api.orcarouter.ai/v1", Models: 117},
		{ID: "anthropic", Name: "Anthropic", Type: "anthropic", Models: 17},
	}}})
	_ = cmd
	out := ansi.ReplaceAllString(v.render(160), "")
	for _, want := range []string{"Other: enter the API", "OrcaRouter (orcarouter) · 117 models · OpenAI API", "2026-10-29"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker lacks %q:\n%s", want, out)
		}
	}
	price := struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	}{0.15, 0.6}
	v.update(catalogModelsLoaded{p: client.CatalogModels{
		CatalogProvider: client.CatalogProvider{ID: "orcarouter", Name: "OrcaRouter", Type: "openai",
			BaseURL: "https://api.orcarouter.ai/v1", Env: []string{"ORCAROUTER_API_KEY"}},
		Models: []client.CatalogModel{{ID: "deepseek/deepseek-v4-flash", Context: 1048576, ToolCall: true, Cost: &price}},
	}})
	out = ansi.ReplaceAllString(v.render(160), "")
	for _, want := range []string{"https://api.orcarouter.ai/v1", "ORCAROUTER_API_KEY", "deepseek/deepseek-v4-flash  1048k ctx  $0.15/$0.6 per M"} {
		if !strings.Contains(out, want) {
			t.Errorf("model form lacks %q:\n%s", want, out)
		}
	}
	// The existing provider's listed models start chosen.
	if v.name != "orcarouter" || len(v.catModel) != 1 || v.catModel[0] != "deepseek/deepseek-v4.1-flash-1017" {
		t.Errorf("name %q models %v", v.name, v.catModel)
	}
	for in, want := range map[string]string{"llama.cpp": "llama-cpp", "Some_Provider": "some-provider", "-x-": "x"} {
		if got := providerName(in); got != want {
			t.Errorf("providerName(%q) = %q, want %q", in, got, want)
		}
	}
}

// profileAPI is a fake System API that records the PUT of the profiles.
func profileAPI(t *testing.T) (*client.Client, *router.ProfilesFile) {
	t.Helper()
	got := &router.ProfilesFile{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/router/profiles":
			_ = json.NewDecoder(r.Body).Decode(got)
		case r.URL.Path == "/models":
			_, _ = w.Write([]byte(`[{"id":"qwen"},{"id":"gemma"},{"id":"embed","settings":{"embedding":true}}]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, got
}

// finish submits the open form and runs the API call.
func finish(v *routerView) {
	v.form = nil
	for _, m := range drain(v.submit()) {
		v.update(m)
	}
}

func TestRouterProfileLocalModel(t *testing.T) {
	c, got := profileAPI(t)
	v := newRouterView(c)
	v.update(routerFixture())
	for _, m := range drain(v.keyPress("m")) {
		v.update(m)
	}
	if v.mode != "profilepick" || v.pf.profile != "auto" {
		t.Fatalf("mode %q profile %q", v.mode, v.pf.profile)
	}
	v.pf.action = "settings"
	v.form = nil
	v.submit()
	if v.mode != "profile" || v.pf.model != "qwen" {
		t.Fatalf("mode %q model %q", v.mode, v.pf.model)
	}
	v.pf.model, v.pf.fallback = "gemma", "local orcarouter/deepseek/deepseek-v4.1-flash-1017"
	finish(v)
	p := got.Profiles["auto"]
	if p.LocalModel != "gemma" || len(p.Fallback) != 2 || len(p.Rules) != 2 {
		t.Errorf("profile %+v", p)
	}
	// The settings that the form does not show are kept.
	if r := got.Calibration.SampleRate; r == nil || *r != 0.2 {
		t.Errorf("calibration lost: %+v", got.Calibration)
	}
	if s := p.Rules[0].When.Sensitive; s == nil || !*s {
		t.Errorf("rule condition lost: %+v", p.Rules[0])
	}
}

func TestRouterRuleEdits(t *testing.T) {
	c, got := profileAPI(t)
	v := newRouterView(c)
	v.update(routerFixture())

	// Move the second rule up.
	v.keyPress("r")
	v.pf.pick, v.pf.action = "auto\x001", "up"
	finish(v)
	if r := got.Profiles["auto"].Rules; len(r) != 2 || r[0].Name != "long" {
		t.Fatalf("rules after move %+v", r)
	}

	// Add a rule.
	v.keyPress("r")
	v.pf.pick = "auto\x00" + addRule
	v.form = nil
	v.submit()
	if v.mode != "rule" {
		t.Fatalf("mode %q, want rule", v.mode)
	}
	v.pf.ruleName, v.pf.route = "hard code", "orcarouter/deepseek/deepseek-v4.1-flash-1017"
	v.pf.tasks, v.pf.diffs, v.pf.sensitive = []string{"code"}, []string{"hard"}, "false"
	finish(v)
	r := got.Profiles["auto"].Rules
	if len(r) != 3 || r[2].Name != "hard code" || r[2].When.Task[0] != "code" || r[2].When.Sensitive == nil || *r[2].When.Sensitive {
		t.Fatalf("rules after add %+v", r)
	}

	// Edit the first rule: its prompt condition stays when the field is kept.
	v.keyPress("r")
	v.pf.pick, v.pf.action = "auto\x001", "edit"
	v.form = nil
	v.submit()
	if v.pf.promptOver != "local_context" {
		t.Fatalf("prompt field %q", v.pf.promptOver)
	}
	v.pf.ruleName = "too long"
	finish(v)
	if r := got.Profiles["auto"].Rules[1]; r.Name != "too long" || r.When.PromptTokensOver == nil || !r.When.PromptTokensOver.LocalContext {
		t.Errorf("edited rule %+v", r)
	}

	// Delete it.
	v.keyPress("r")
	v.pf.pick, v.pf.action = "auto\x000", "delete"
	finish(v)
	if r := got.Profiles["auto"].Rules; len(r) != 1 || r[0].Name != "long" {
		t.Errorf("rules after delete %+v", r)
	}
}

func TestRouterCalibrationSettings(t *testing.T) {
	c, got := profileAPI(t)
	v := newRouterView(c)
	v.update(routerFixture())
	v.keyPress("c")
	if v.pf.rate != "20" || v.pf.daily != "1" || !v.pf.seed {
		t.Fatalf("form values %+v", v.pf)
	}
	v.pf.rate, v.pf.daily, v.pf.judge, v.pf.autoApply, v.pf.seed = "5", "0.5", "local", true, false
	finish(v)
	cc := got.Calibration
	if cc.SampleRate == nil || *cc.SampleRate != 0.05 || cc.Daily() != 0.5 || cc.Judge != "local" || !cc.AutoApply || !cc.NoSeed {
		t.Errorf("calibration %+v", cc)
	}
	if got.Profiles["auto"].LocalModel != "qwen" {
		t.Errorf("profiles lost: %+v", got.Profiles)
	}
}

func TestRouterClassifierAndNewProfile(t *testing.T) {
	c, got := profileAPI(t)
	v := newRouterView(c)
	v.update(routerFixture())
	for _, m := range drain(v.keyPress("s")) {
		v.update(m)
	}
	if v.mode != "classifier" || !v.pf.useStarter {
		t.Fatalf("mode %q starter %v", v.mode, v.pf.useStarter)
	}
	v.pf.words, v.pf.useStarter, v.pf.clsModel = "project-x, acme corp", false, "embed"
	finish(v)
	if len(got.SensitiveWords) != 2 || got.SensitiveWords[1] != "acme corp" || !got.Classifier.NoStarter || got.Classifier.Model != "embed" {
		t.Errorf("classifier %+v words %q", got.Classifier, got.SensitiveWords)
	}

	for _, m := range drain(v.keyPress("m")) {
		v.update(m)
	}
	v.pf.profile = newProfile
	v.form = nil
	v.submit()
	v.pf.name, v.pf.model = "coding", "gemma"
	finish(v)
	if p, ok := got.Profiles["coding"]; !ok || p.LocalModel != "gemma" || len(got.Profiles) != 2 {
		t.Errorf("profiles %+v", got.Profiles)
	}
}

func TestProviderChangeKeepsPrices(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/router/providers/orcarouter" {
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newRouterView(c)
	fx := routerFixture()
	fx.r.Providers[0].Prices = map[string]router.Price{"deepseek/deepseek-v4.1-flash-1017": {Input: 0.44, Output: 1.32}}
	v.update(fx)
	v.mode, v.name, v.ptype, v.baseURL, v.mods = "provider", "orcarouter", "openai", "https://api.orcarouter.ai/v1", "deepseek/deepseek-v4.1-flash-1017"
	finish(v)
	if p, _ := got["prices"].(map[string]any); p["deepseek/deepseek-v4.1-flash-1017"] == nil {
		t.Errorf("prices lost: %v", got)
	}
}
