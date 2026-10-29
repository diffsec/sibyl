package router

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func hash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

// fakeUpstream is a minimal llama-swap or OpenAI-compatible provider. It
// records each chat request body and answers with fixed responses.
type fakeUpstream struct {
	name string
	mu   sync.Mutex
	got  []map[string]any
	auth []string
	// status, when set, is returned for every chat request.
	status int
}

func (f *fakeUpstream) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return nil
	}
	return f.got[len(f.got)-1]
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	switch path {
	case "/running":
		_, _ = io.WriteString(w, `{"running":[{"model":"small","state":"ready"}]}`)
		return
	case "/models":
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"small","object":"model"},{"id":"embed","object":"model"}]}`)
		return
	case "/rerank":
		_, _ = fmt.Fprintf(w, `{"auth":%q}`, r.Header.Get("Authorization"))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.got = append(f.got, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status := f.status
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Retry-After", "0")
		http.Error(w, `{"error":{"message":"busy"}}`, status)
		return
	}
	switch path {
	case "/embeddings":
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`)
	case "/chat/completions":
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{
				`{"choices":[{"delta":{"content":"Hel"}}]}`,
				`{"choices":[{"delta":{"content":"lo"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get","arguments":"{\"a\""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
			} {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"from %s","tool_calls":[{"id":"c1","function":{"name":"get","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, f.name)
	default:
		http.NotFound(w, r)
	}
}

type harness struct {
	srv       *httptest.Server
	local     *fakeUpstream
	provider  *fakeUpstream
	anthropic *fakeAnthropic
	dir       string
	router    *Server
}

const (
	adminKey   = "sib-admin"
	defaultKey = "sib-default"
	owuiKey    = "sib-owui"
	hybridKey  = "sib-hybrid"
	localKey   = "sib-local"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newHarness(t *testing.T, profiles string) *harness {
	t.Helper()
	h := &harness{local: &fakeUpstream{name: "local"}, provider: &fakeUpstream{name: "cloud"},
		anthropic: &fakeAnthropic{}, dir: t.TempDir()}
	antSrv := httptest.NewTLSServer(h.anthropic)
	t.Cleanup(antSrv.Close)
	localSrv := httptest.NewServer(h.local)
	t.Cleanup(localSrv.Close)
	provSrv := httptest.NewTLSServer(h.provider)
	t.Cleanup(provSrv.Close)

	write(t, filepath.Join(h.dir, ClientsName), fmt.Sprintf(`clients:
  - name: laptop
    key_sha256: %s
    policy: hybrid
    allow_hint: true
  - name: homeauto
    key_sha256: %s
    policy: local-only
`, hash(hybridKey), hash(localKey)))
	write(t, filepath.Join(h.dir, ProvidersName), fmt.Sprintf(`providers:
  cloud:
    type: openai
    base_url: %s/v1
    models: [big]
  claude:
    type: anthropic
    base_url: %s
`, provSrv.URL, antSrv.URL))
	write(t, filepath.Join(h.dir, KeysDir, "cloud"), "provider-secret\n")
	write(t, filepath.Join(h.dir, KeysDir, "claude"), "anthropic-secret\n")
	write(t, filepath.Join(h.dir, ModelsName), `[{"id":"small","context":1000,
		"presets":{"coding":{"temperature":0.6,"top_k":20},"writing":{"temperature":1,"min_p":0.05}}},
		{"id":"embed","embedding":true}]`)
	if profiles != "" {
		write(t, filepath.Join(h.dir, ProfilesName), profiles)
	}
	h.router = &Server{
		ConfigDir: h.dir, LocalURL: localSrv.URL, AdminKey: adminKey, DefaultKey: defaultKey, OpenWebUIKey: owuiKey,
		HTTP: provSrv.Client(), Log: &Log{Path: filepath.Join(h.dir, "decisions.jsonl")},
		Spend:  &Spend{Path: filepath.Join(h.dir, "usage.json")},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := h.router.Reload(); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(h.router.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

const testProfiles = `sensitive_words: [project-x]
profiles:
  auto:
    local_model: small
    rules:
      - name: secrets stay
        when: {sensitive: true}
        route: local
      - name: too long
        when: {prompt_tokens_over: local_context}
        route: cloud/big
      - name: images
        when: {has_images: true}
        route: cloud/vision
      - name: asked for cloud
        when: {hint: [cloud]}
        route: cloud/big
      - name: everything else
        route: local
    fallback: [local, cloud/big]
`

func (h *harness) post(t *testing.T, path, key string, body any, hdr ...string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(string(b)))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func chatBody(model, text string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": text}}}
}

func TestAuth(t *testing.T) {
	h := newHarness(t, "")
	for _, c := range []struct {
		key  string
		want int
	}{{"", 401}, {"wrong", 401}, {adminKey, 401}, {defaultKey, 200}, {owuiKey, 200}, {hybridKey, 200}, {localKey, 200}} {
		resp := h.post(t, "/v1/chat/completions", c.key, chatBody("small", "hi"))
		if resp.StatusCode != c.want {
			t.Errorf("key %q: status %d, want %d", c.key, resp.StatusCode, c.want)
		}
	}
	// Anthropic clients send x-api-key.
	resp := h.post(t, "/v1/messages", "", map[string]any{"model": "small", "max_tokens": 10,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "X-Api-Key", hybridKey)
	if resp.StatusCode != 200 {
		t.Errorf("x-api-key: status %d", resp.StatusCode)
	}
}

func TestLocalRequestIsUnchanged(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", defaultKey, chatBody("small", "hi"))
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "from local") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	if got := resp.Header.Get("X-Sibyl-Route"); got != "local/small" {
		t.Errorf("route header %q", got)
	}
	if h.local.auth[0] != "" {
		t.Errorf("the client key reached llama-swap: %q", h.local.auth[0])
	}
}

func TestProviderRoute(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("cloud/big", "hi"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if h.provider.last()["model"] != "big" {
		t.Errorf("provider got model %v", h.provider.last()["model"])
	}
	if h.provider.auth[0] != "Bearer provider-secret" {
		t.Errorf("provider auth %q", h.provider.auth[0])
	}

	// Local-only clients, and the built-in clients by default, cannot name
	// a provider model.
	for _, key := range []string{localKey, defaultKey, owuiKey} {
		resp := h.post(t, "/v1/chat/completions", key, chatBody("cloud/big", "hi"))
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("key %q: status %d, want 403", key, resp.StatusCode)
		}
	}
	// The hint local narrows a hybrid client too.
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("cloud/big", "hi"), "X-Sibyl-Route", "local")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("hint local: status %d, want 403", resp.StatusCode)
	}
	if h.provider.count() != 1 {
		t.Errorf("provider got %d requests, want 1", h.provider.count())
	}
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("nowhere/big", "hi"))
	if resp.StatusCode != http.StatusNotFound || h.local.count() != 0 {
		t.Errorf("unknown provider: status %d, local requests %d", resp.StatusCode, h.local.count())
	}
}

func TestAutoRules(t *testing.T) {
	h := newHarness(t, testProfiles)
	long := strings.Repeat("word ", 1000)
	image := map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,AAAA"}},
	}}}}
	for _, c := range []struct {
		name string
		key  string
		body map[string]any
		hint string
		want string
	}{
		{"short goes local", hybridKey, chatBody("auto", "hi"), "", "local/small"},
		{"long goes to the provider", hybridKey, chatBody("auto", long), "", "cloud/big"},
		{"long and secret stays", hybridKey, chatBody("auto", long+" -----BEGIN RSA PRIVATE KEY-----"), "", "local/small"},
		{"owner word stays", hybridKey, chatBody("auto", long+" Project-X"), "", "local/small"},
		{"image goes to vision", hybridKey, image, "", "cloud/vision"},
		{"hint cloud", hybridKey, chatBody("auto", "hi"), "cloud", "cloud/big"},
		{"local-only long stays", localKey, chatBody("auto", long), "", "local/small"},
		{"local-only image stays", localKey, image, "", "local/small"},
		{"hint local stays", hybridKey, chatBody("auto", long), "local", "local/small"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := h.post(t, "/v1/chat/completions", c.key, c.body, "X-Sibyl-Route", c.hint)
			got, _, _ := strings.Cut(resp.Header.Get("X-Sibyl-Route"), ";")
			if resp.StatusCode != 200 || got != c.want {
				t.Errorf("status %d route %q, want %q", resp.StatusCode, got, c.want)
			}
		})
	}
}

// TestLocalOnlyNeverLeaves runs every rule type with a provider route
// against a local-only client.
func TestLocalOnlyNeverLeaves(t *testing.T) {
	conds := []string{
		"{}", "{sensitive: false}", "{has_images: false}", "{has_tools: false}", "{json_schema: false}",
		"{stream: false}", "{local_ready: true}", "{prompt_tokens_over: 0}", "{max_tokens_over: 0}",
		"{task: [unknown]}", "{difficulty: [unknown]}", "{hint: [auto, local]}", "{clients: [homeauto, default, open-webui, laptop]}",
	}
	for _, cond := range conds {
		t.Run(cond, func(t *testing.T) {
			h := newHarness(t, fmt.Sprintf(`profiles:
  auto:
    local_model: small
    rules:
      - name: out
        when: %s
        route: cloud/big
    fallback: [cloud/big, cloud/other]
`, cond))
			for _, key := range []string{localKey, defaultKey, owuiKey} {
				for _, hint := range []string{"", "cloud", "local"} {
					h.post(t, "/v1/chat/completions", key, chatBody("auto", "hi"), "X-Sibyl-Route", hint)
					h.post(t, "/v1/messages", key, map[string]any{"model": "auto", "max_tokens": 5,
						"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "X-Sibyl-Route", hint)
				}
			}
			if n := h.provider.count() + h.anthropic.count(); n != 0 {
				t.Fatalf("the provider got %d requests from local-only clients", n)
			}
			// The same rule sends a hybrid client out, so the rule did match.
			probe := chatBody("auto", "hi")
			probe["max_tokens"] = 10
			h.post(t, "/v1/chat/completions", hybridKey, probe)
			if h.provider.count() != 1 {
				t.Fatalf("hybrid client: provider got %d requests, want 1", h.provider.count())
			}
		})
	}
}

func TestFallback(t *testing.T) {
	h := newHarness(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: out
        route: cloud/big
    fallback: [local]
`)
	h.provider.status = http.StatusServiceUnavailable
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	if got := resp.Header.Get("X-Sibyl-Route"); resp.StatusCode != 200 || !strings.HasPrefix(got, "local/small") {
		t.Fatalf("status %d route %q", resp.StatusCode, got)
	}
	// One retry at the provider, then the local model.
	if h.provider.count() != 2 || h.local.count() != 1 {
		t.Errorf("provider %d, local %d requests", h.provider.count(), h.local.count())
	}

	// The last route's error goes to the client.
	h.local.status = http.StatusBadGateway
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}

func TestMessagesNonStream(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/messages", hybridKey, map[string]any{
		"model": "small", "max_tokens": 100, "system": "be brief", "top_k": 5,
		"tools": []any{
			map[string]any{"name": "get", "description": "d", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search_20250305", "name": "web_search"},
		},
		"tool_choice": map[string]any{"type": "any"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "x", "signature": "s"},
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "t1", "name": "get", "input": map[string]any{"a": 1}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "42"},
				map[string]any{"type": "text", "text": "thanks"},
			}},
		},
	})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.Model != "small" || out.StopReason != "tool_use" || len(out.Content) != 2 ||
		out.Content[0].Text != "from local" || out.Content[1].Name != "get" || out.Content[1].Input["a"] != float64(1) ||
		out.Usage["input_tokens"] != 7 || out.Usage["output_tokens"] != 2 {
		t.Errorf("response %s", b)
	}

	got := h.local.last()
	msgs := got["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Errorf("roles %v", roles)
	}
	asst := msgs[2].(map[string]any)
	if asst["content"] != "calling" || len(asst["tool_calls"].([]any)) != 1 {
		t.Errorf("assistant %v", asst)
	}
	if got["tool_choice"] != "required" || len(got["tools"].([]any)) != 1 || got["top_k"] != float64(5) {
		t.Errorf("tools %v choice %v top_k %v", got["tools"], got["tool_choice"], got["top_k"])
	}

	// A provider route gets no top_k.
	h.post(t, "/v1/messages", hybridKey, map[string]any{"model": "cloud/big", "max_tokens": 10, "top_k": 5,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if _, ok := h.provider.last()["top_k"]; ok {
		t.Error("top_k reached the provider")
	}
}

func TestMessagesStream(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/messages", hybridKey, map[string]any{"model": "small", "max_tokens": 10, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var events []string
	var text, args string
	var stop string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, name)
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(data), &ev)
		text += ev.Delta.Text
		args += ev.Delta.PartialJSON
		if ev.Delta.StopReason != "" {
			stop = ev.Delta.StopReason
		}
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if strings.Join(events, ",") != want {
		t.Errorf("events\n%s\nwant\n%s", strings.Join(events, ","), want)
	}
	if text != "Hello" || args != `{"a":1}` || stop != "tool_use" {
		t.Errorf("text %q args %q stop %q", text, args, stop)
	}
	if so, _ := h.local.last()["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Error("the stream did not ask for usage")
	}
}

func TestEmbeddingsAndPassthrough(t *testing.T) {
	h := newHarness(t, testProfiles)
	resp := h.post(t, "/v1/embeddings", owuiKey, map[string]any{"model": "embed", "input": "x"})
	if resp.StatusCode != 200 || h.local.last()["model"] != "embed" {
		t.Errorf("embeddings status %d", resp.StatusCode)
	}
	resp = h.post(t, "/v1/embeddings", owuiKey, map[string]any{"model": "auto", "input": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("auto embeddings status %d, want 400", resp.StatusCode)
	}
	resp = h.post(t, "/v1/rerank", localKey, map[string]any{"model": "small"})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != `{"auth":""}` {
		t.Errorf("rerank status %d body %s", resp.StatusCode, b)
	}
}

func TestModelsList(t *testing.T) {
	h := newHarness(t, testProfiles)
	ids := func(key string) string {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		var s []string
		for _, d := range out.Data {
			s = append(s, d.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(hybridKey); got != "small,embed,auto,small:coding,small:writing,cloud/big" {
		t.Errorf("hybrid: %s", got)
	}
	if got := ids(localKey); got != "small,embed,auto,small:coding,small:writing" {
		t.Errorf("local-only: %s", got)
	}
}

func TestExplain(t *testing.T) {
	h := newHarness(t, testProfiles)
	req := map[string]any{"client": "laptop", "request": chatBody("auto", strings.Repeat("x", 4000))}
	if resp := h.post(t, "/sibyl/explain", hybridKey, req); resp.StatusCode != 401 {
		t.Errorf("a client key called explain: %d", resp.StatusCode)
	}
	resp := h.post(t, "/sibyl/explain", adminKey, req)
	var d struct {
		Route    string       `json:"route"`
		Rule     string       `json:"rule"`
		Fallback []string     `json:"fallback"`
		Rules    []RuleResult `json:"rules"`
		Features Features     `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.Route != "cloud/big" || d.Rule != "too long" || len(d.Rules) != 5 || !d.Rules[1].Matched ||
		d.Rules[0].Reason != "sensitive is false" || d.Features.LocalContext != 1000 || d.Features.PromptTokens != 1334 {
		t.Errorf("explain %+v", d)
	}
	if strings.Join(d.Fallback, ",") != "local/small" {
		t.Errorf("fallback %v", d.Fallback)
	}
	if h.provider.count()+h.local.count() != 0 {
		t.Error("explain sent the request")
	}
}

func TestDecisionLog(t *testing.T) {
	h := newHarness(t, testProfiles)
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "my password=hunter2hunter2"))
	_, _ = io.ReadAll(resp.Body)
	// The handler writes the entry after the response ends.
	var b []byte
	var err error
	for i := 0; i < 100; i++ {
		if b, err = os.ReadFile(filepath.Join(h.dir, "decisions.jsonl")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if e.Client != "laptop" || e.Route != "local/small" || e.Rule != "secrets stay" || e.Status != 200 ||
		e.PromptTokens != 7 || e.CompletionTokens != 2 {
		t.Errorf("entry %+v", e)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Error("the log holds message content")
	}
}

func TestReloadKeepsLastGoodConfig(t *testing.T) {
	h := newHarness(t, testProfiles)
	write(t, filepath.Join(h.dir, ProfilesName), "profiles:\n  auto:\n    local_model: small\n    rules:\n      - name: bad\n        route: nowhere/x\n")
	time.Sleep(10 * time.Millisecond)
	if err := h.router.Reload(); err == nil || !strings.Contains(err.Error(), `no provider "nowhere"`) {
		t.Fatalf("err %v", err)
	}
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	if resp.StatusCode != 200 {
		t.Errorf("status %d after a bad reload", resp.StatusCode)
	}
}

func TestValidate(t *testing.T) {
	provs := ProvidersFile{Providers: map[string]Provider{"cloud": {Type: ProviderOpenAI, BaseURL: "https://x/v1"}}}
	for _, c := range []struct {
		name string
		err  string
		f    func() error
	}{
		{"bad policy", "policy must be", func() error {
			return ClientsFile{Clients: []Client{{Name: "a", Policy: "open", KeySHA256: hash("k")}}}.Validate()
		}},
		{"builtin key", "Podman secret", func() error {
			return ClientsFile{Clients: []Client{{Name: "default", Policy: Hybrid, KeySHA256: hash("k")}}}.Validate()
		}},
		{"missing hash", "key_sha256", func() error {
			return ClientsFile{Clients: []Client{{Name: "a", Policy: Hybrid}}}.Validate()
		}},
		{"http provider", "https://", func() error {
			return ProvidersFile{Providers: map[string]Provider{"p": {Type: ProviderOpenAI, BaseURL: "http://x"}}}.Validate()
		}},
		{"unknown type", "type must be", func() error {
			return ProvidersFile{Providers: map[string]Provider{"p": {Type: "gemini", BaseURL: "https://x"}}}.Validate()
		}},
		{"negative price", "negative", func() error {
			return ProvidersFile{Providers: map[string]Provider{"p": {Type: ProviderAnthropic,
				Prices: map[string]Price{"m": {Input: -1}}}}}.Validate()
		}},
		{"negative budget", "negative", func() error {
			return ClientsFile{Clients: []Client{{Name: "a", Policy: Hybrid, KeySHA256: hash("k"), Budget: &Budget{DailyUSD: -1}}}}.Validate()
		}},
		{"reserved", "reserved", func() error {
			return ProvidersFile{Providers: map[string]Provider{"local": {Type: ProviderOpenAI, BaseURL: "https://x"}}}.Validate()
		}},
		{"bad task", "task", func() error {
			return ProfilesFile{Profiles: map[string]Profile{"auto": {LocalModel: "m",
				Rules: []Rule{{Name: "r", When: When{Task: []string{"poetry"}}, Route: "local"}}}}}.Validate(provs)
		}},
		{"no local model", "local_model", func() error {
			return ProfilesFile{Profiles: map[string]Profile{"auto": {}}}.Validate(provs)
		}},
		{"bad fallback", "fallback", func() error {
			return ProfilesFile{Profiles: map[string]Profile{"auto": {LocalModel: "m", Fallback: []string{"x/y"}}}}.Validate(provs)
		}},
	} {
		if err := c.f(); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.err)
		}
	}
	ok := ProfilesFile{Profiles: map[string]Profile{"auto": {LocalModel: "m", Rules: []Rule{
		{Name: "r", When: When{Sensitive: ptr(true), PromptTokensOver: &Tokens{LocalContext: true}}, Route: "cloud/big"},
	}}}}
	if err := ok.Validate(provs); err != nil {
		t.Error(err)
	}
}

func TestTokensYAMLAndJSON(t *testing.T) {
	var w When
	if err := json.Unmarshal([]byte(`{"prompt_tokens_over":"local_context","max_tokens_over":5}`), &w); err != nil {
		t.Fatal(err)
	}
	if !w.PromptTokensOver.LocalContext || *w.MaxTokensOver != 5 {
		t.Errorf("%+v", w)
	}
	if err := json.Unmarshal([]byte(`{"prompt_tokens_over":120}`), &w); err != nil || w.PromptTokensOver.N != 120 {
		t.Errorf("%v %+v", err, w)
	}
	if err := json.Unmarshal([]byte(`{"prompt_tokens_over":"lots"}`), &w); err == nil {
		t.Error("lots parsed")
	}
}

func TestSensitive(t *testing.T) {
	for _, s := range []string{
		"-----BEGIN OPENSSH PRIVATE KEY-----", "AKIAABCDEFGHIJKLMNOP", "ghp_" + strings.Repeat("a", 36),
		"sk-ant-api03-" + strings.Repeat("b", 30), "xoxb-1234567890-abc", "api_key: abcdefgh12345",
	} {
		if !Sensitive("text "+s+" text", nil) {
			t.Errorf("%q not found", s)
		}
	}
	for _, s := range []string{"hello world", "the password is long", "sk-short"} {
		if Sensitive(s, nil) {
			t.Errorf("%q found", s)
		}
	}
	if !Sensitive("about Falcon soon", []string{"falcon"}) {
		t.Error("word not found")
	}
}

func TestPresetRequests(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", localKey, chatBody("small:coding", "hi"))
	if resp.StatusCode != 200 || resp.Header.Get("X-Sibyl-Route") != "local/small" {
		t.Fatalf("status %d route %q", resp.StatusCode, resp.Header.Get("X-Sibyl-Route"))
	}
	got := h.local.last()
	if got["model"] != "small" || got["temperature"] != 0.6 || got["top_k"] != float64(20) {
		t.Errorf("request %v", got)
	}
	// The client's own value wins.
	body := chatBody("small:writing", "hi")
	body["temperature"] = 0.2
	h.post(t, "/v1/chat/completions", localKey, body)
	if got := h.local.last(); got["temperature"] != 0.2 || got["min_p"] != 0.05 {
		t.Errorf("request %v", got)
	}
	// Messages requests get the preset too.
	h.post(t, "/v1/messages", localKey, map[string]any{"model": "small:coding", "max_tokens": 5,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if got := h.local.last(); got["temperature"] != 0.6 {
		t.Errorf("messages request %v", got)
	}
	if resp := h.post(t, "/v1/chat/completions", localKey, chatBody("small:poetry", "hi")); resp.StatusCode != 404 {
		t.Errorf("unknown preset: %d", resp.StatusCode)
	}
}
