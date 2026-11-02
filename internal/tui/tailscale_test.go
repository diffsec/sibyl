package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/hostd"
)

func TestTailscaleText(t *testing.T) {
	for _, c := range []struct {
		st   *hostd.TailscaleState
		want string
	}{
		{nil, "not in this host image"},
		{&hostd.TailscaleState{}, "press t to join"},
		{&hostd.TailscaleState{Enabled: true, Backend: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/x"}, "https://login.tailscale.com/a/x"},
		{&hostd.TailscaleState{Enabled: true, Backend: hostd.NeedsMachineAuth}, "https://login.tailscale.com/admin/machines"},
		{&hostd.TailscaleState{Enabled: true, Backend: "Running", Tailnet: "ben@example.com", DNSName: "sibyl.t.ts.net", IPs: []string{"100.64.0.5"}},
			"sibyl.t.ts.net  100.64.0.5"},
	} {
		if got := ansi.Strip(tailscaleText(c.st)); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q", c.st, got)
		}
	}
	links := serviceLinks(hostd.State{Tailscale: &hostd.TailscaleState{Backend: "Running", IPs: []string{"100.64.0.5", "fd7a::1"}}})
	if len(links) != 4 || links[0].url != "https://100.64.0.5/" {
		t.Errorf("links %v", links)
	}
}

func TestTailscaleForm(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		got = append(got, r.Method+" "+r.URL.Path+" "+toJSON(b))
		_, _ = w.Write([]byte(`{"ok":true,"message":"done"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v := newHostView(c)
	v.st.Tailscale = &hostd.TailscaleState{}
	v.tailscaleForm()
	if v.form == nil || v.mode != "tsup" {
		t.Fatalf("t did not open the join form: %q", v.mode)
	}
	v.tsHow, v.tsKey = "key", " tskey-auth-kAbC123-secretpart "
	v.form = nil
	drain(v.submit())
	if v.tsKey != "" {
		t.Error("the key stayed in the view")
	}
	v.st.Tailscale = &hostd.TailscaleState{Enabled: true, Backend: "Running", Tailnet: "ben@example.com"}
	v.tailscaleForm()
	if v.mode != "tsdown" {
		t.Fatalf("a running host did not offer to leave: %q", v.mode)
	}
	v.confirm = true
	v.form = nil
	drain(v.submit())
	mu.Lock()
	defer mu.Unlock()
	want := []string{`POST /host/tailscale {"auth_key":"tskey-auth-kAbC123-secretpart"}`, `DELETE /host/tailscale {}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s", strings.Join(got, "\n"))
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
