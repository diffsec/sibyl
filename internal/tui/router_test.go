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
	r.Profiles.Profiles = map[string]client.RouterProfile{"auto": {LocalModel: "qwen", Rules: []client.RouterRule{
		{Name: "secrets", Route: "local"}, {Name: "long", Route: "orcarouter/deepseek/deepseek-v4.1-flash-1017"}}}}
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
		"auto", "local qwen · 2 rules → local, orcarouter/deepseek/deepseek-v4.1-flash-1017",
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
