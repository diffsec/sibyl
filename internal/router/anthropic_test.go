package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAnthropic is a minimal Messages API. It records each request.
type fakeAnthropic struct {
	mu     sync.Mutex
	bodies []map[string]any
	raw    []string
	header []http.Header
	status int
}

func (f *fakeAnthropic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeAnthropic) last() (map[string]any, string, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.bodies) - 1
	return f.bodies[n], f.raw[n], f.header[n]
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.bodies, f.raw, f.header = append(f.bodies, body), append(f.raw, string(b)), append(f.header, r.Header.Clone())
	status := f.status
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		return
	}
	if body["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[
			{"type":"thinking","thinking":"hm","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"toolu_1","name":"get","input":{"a":1}}],
			"stop_reason":"tool_use","stop_sequence":null,
			"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}`, body["model"])
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range []string{
		`message_start`, `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`content_block_start`, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`content_block_delta`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`content_block_stop`, `{"type":"content_block_stop","index":0}`,
		`content_block_start`, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"get","input":{}}}`,
		`content_block_delta`, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\""}}`,
		`content_block_delta`, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":2}"}}`,
		`content_block_stop`, `{"type":"content_block_stop","index":1}`,
		`message_delta`, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`,
		`message_stop`, `{"type":"message_stop"}`,
	} {
		if !strings.HasPrefix(ev, "{") {
			_, _ = fmt.Fprintf(w, "event: %s\n", ev)
			continue
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
	}
}

func TestAnthropicAdapterRequest(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", hybridKey, map[string]any{
		"model": "claude/claude-sonnet-5-5", "temperature": 0.2, "top_p": 0.9, "reasoning_effort": "high",
		"stop": "END", "parallel_tool_calls": false, "tool_choice": "auto",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get", "description": "get a thing",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "integer"}},
				"required": []any{"a"}, "additionalProperties": false},
		}}},
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,AAAA"}},
			}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "get", "arguments": `{"a":1}`}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "get", "arguments": `{"a":2}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "one"},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": "two"},
			map[string]any{"role": "user", "content": "and?"},
		},
	})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if got := resp.Header.Get("X-Sibyl-Dropped"); got != "temperature,top_p" {
		t.Errorf("dropped %q", got)
	}
	got, _, hdr := h.anthropic.last()
	if hdr.Get("X-Api-Key") != "anthropic-secret" || hdr.Get("Authorization") != "" {
		t.Errorf("auth headers %v", hdr)
	}
	if got["model"] != "claude-sonnet-5-5" || got["max_tokens"] != float64(defaultMaxTokens) ||
		got["temperature"] != nil || got["top_p"] != nil {
		t.Errorf("request %v", got)
	}
	if cc, _ := got["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("cache_control %v", got["cache_control"])
	}
	if oc, _ := got["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config %v", got["output_config"])
	}
	if sys := got["system"].([]any); len(sys) != 1 || sys[0].(map[string]any)["text"] != "be brief" {
		t.Errorf("system %v", got["system"])
	}
	if stops := got["stop_sequences"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop %v", got["stop_sequences"])
	}
	tc := got["tool_choice"].(map[string]any)
	if tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice %v", tc)
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "get" || schema["type"] != "object" || schema["additionalProperties"] != false || len(schema["required"].([]any)) != 1 {
		t.Errorf("tool %v", tool)
	}
	msgs := got["messages"].([]any)
	var shape []string
	for _, m := range msgs {
		mm := m.(map[string]any)
		var types []string
		for _, c := range mm["content"].([]any) {
			types = append(types, c.(map[string]any)["type"].(string))
		}
		shape = append(shape, mm["role"].(string)+":"+strings.Join(types, "+"))
	}
	// The two tool results and the next user text merge into one turn.
	if want := "user:text+image,assistant:tool_use+tool_use,user:tool_result+tool_result+text"; strings.Join(shape, ",") != want {
		t.Errorf("messages %s, want %s", strings.Join(shape, ","), want)
	}
	img := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)["source"].(map[string]any)
	if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "AAAA" {
		t.Errorf("image %v", img)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	m := out.Choices[0]
	if m.Message.Content != "hello" || m.Message.ReasoningContent != "hm" || m.FinishReason != "tool_calls" ||
		len(m.Message.ToolCalls) != 1 || m.Message.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("response %s", b)
	}
	if out.Usage["prompt_tokens"] != float64(130) || out.Usage["completion_tokens"] != float64(5) {
		t.Errorf("usage %v", out.Usage)
	}
}

func TestAnthropicAdapterStream(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", hybridKey, map[string]any{"model": "claude/claude-sonnet-5-5", "stream": true,
		"stream_options": map[string]bool{"include_usage": true}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got, _, _ := h.anthropic.last(); got["max_tokens"] != float64(defaultMaxTokensStream) {
		t.Errorf("max_tokens %v", got["max_tokens"])
	}
	var text, args, finish string
	var usage map[string]any
	done := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("chunk %s: %v", data, err)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			for _, tc := range ch.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text != "Hello" || args != `{"a":2}` || finish != "tool_calls" || !done ||
		usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) {
		t.Errorf("text %q args %q finish %q done %v usage %v", text, args, finish, done, usage)
	}
}

func TestAnthropicErrorFallsBack(t *testing.T) {
	h := newHarness(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: out
        route: claude/claude-sonnet-5-5
    fallback: [local]
`)
	h.anthropic.status = http.StatusServiceUnavailable
	resp := h.post(t, "/v1/chat/completions", hybridKey, map[string]any{"model": "auto", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if got := resp.Header.Get("X-Sibyl-Route"); resp.StatusCode != 200 || !strings.HasPrefix(got, "local/small") {
		t.Errorf("status %d route %q", resp.StatusCode, got)
	}
	if h.anthropic.count() != 2 {
		t.Errorf("anthropic got %d requests, want 2: one retry", h.anthropic.count())
	}
}

func TestMessagesPassthroughToAnthropic(t *testing.T) {
	h := newHarness(t, "")
	body := `{"model":"claude/claude-opus-5-5","max_tokens":50,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", hybridKey)
	req.Header.Set("Anthropic-Version", "2026-10-21")
	req.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"type":"message"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got, raw, hdr := h.anthropic.last()
	if got["model"] != "claude-opus-5-5" || got["thinking"] == nil || !strings.Contains(raw, `"cache_control"`) {
		t.Errorf("body %s", raw)
	}
	if hdr.Get("X-Api-Key") != "anthropic-secret" || hdr.Get("Anthropic-Version") != "2026-10-21" ||
		hdr.Get("Anthropic-Beta") != "some-beta-2026-01-01" {
		t.Errorf("headers %v", hdr)
	}
	e := lastEntry(t, h)
	// 10 uncached, 100 cache reads, 20 cache writes, 5 out at Opus 5.5 prices.
	want := (10*4 + 100*0.20 + 20*5 + 5*20) / 1e6
	if e.Route != "claude/claude-opus-5-5" || e.PromptTokens != 130 || e.CacheReadTokens != 100 ||
		e.CacheWriteTokens != 20 || math.Abs(e.CostUSD-want) > 1e-12 {
		t.Errorf("entry %+v, want cost %v", e, want)
	}
}

// lastEntry waits for the decision log's last line.
func lastEntry(t *testing.T, h *harness) Entry {
	t.Helper()
	var e Entry
	for i := 0; i < 100; i++ {
		b, err := os.ReadFile(filepath.Join(h.dir, "decisions.jsonl"))
		if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); err == nil && lines[0] != "" {
			if json.Unmarshal([]byte(lines[len(lines)-1]), &e) == nil {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no decision log entry")
	return e
}

func TestPriceCost(t *testing.T) {
	p := AnthropicPrices["claude-haiku-5-5"]
	short := p.Cost(Usage{Prompt: 1000, Completion: 1000})
	if math.Abs(short-(1000*0.10+1000*0.50)/1e6) > 1e-12 {
		t.Errorf("short %v", short)
	}
	long := p.Cost(Usage{Prompt: 200_000, Completion: 1000, CacheRead: 100_000})
	want := (100_000*0.50 + 100_000*0.05 + 1000*2.50) / 1e6
	if math.Abs(long-want) > 1e-12 {
		t.Errorf("long %v, want %v", long, want)
	}
}

func TestSpend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s := &Spend{Path: path}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	_ = s.Add("a", 1, now)
	_ = s.Add("a", 2, now.AddDate(0, 0, -1))
	_ = s.Add("a", 4, now.AddDate(0, -1, 0))
	_ = s.Add("a", 8, now.AddDate(0, 0, -100))
	_ = s.Add("b", 16, now)
	// A new tracker reads the file.
	day, month := (&Spend{Path: path}).Spent("a", now)
	if day != 1 || month != 3 {
		t.Errorf("day %v month %v", day, month)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "2026-07-") {
		t.Errorf("old days kept: %s", b)
	}
}

func TestBudgets(t *testing.T) {
	h := newHarness(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: out
        route: cloud/big
`)
	write(t, filepath.Join(h.dir, ClientsName), fmt.Sprintf(`clients:
  - name: laptop
    key_sha256: %s
    policy: hybrid
    budget: {daily_usd: 1}
`, hash(hybridKey)))
	write(t, filepath.Join(h.dir, ProvidersName), strings.Replace(readFile(t, filepath.Join(h.dir, ProvidersName)),
		"models: [big]", "models: [big]\n    prices: {big: {input: 1000000, output: 1000000}}", 1))
	if err := h.router.Reload(); err != nil {
		t.Fatal(err)
	}
	// The first request costs 7 + 2 tokens at $1 each: over the budget.
	resp := h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	if got := resp.Header.Get("X-Sibyl-Route"); !strings.HasPrefix(got, "cloud/big") {
		t.Fatalf("first request route %q", got)
	}
	_, _ = io.ReadAll(resp.Body)
	if e := lastEntry(t, h); e.CostUSD != 9 {
		t.Fatalf("cost %v", e.CostUSD)
	}
	// Spent: auto falls back to the profile's local model, which the
	// profile's fallback list does not name.
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("auto", "hi"))
	if got := resp.Header.Get("X-Sibyl-Route"); resp.StatusCode != 200 || !strings.HasPrefix(got, "local/small") {
		t.Errorf("after the budget: status %d route %q", resp.StatusCode, got)
	}
	// A direct provider request gets 429.
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("cloud/big", "hi"))
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "daily budget") {
		t.Errorf("direct: status %d %s", resp.StatusCode, b)
	}
	if h.provider.count() != 1 {
		t.Errorf("provider got %d requests, want 1", h.provider.count())
	}
	// A model with no price cannot be used by a client with a budget.
	_ = os.Remove(filepath.Join(h.dir, "usage.json"))
	h.router.Spend = &Spend{Path: filepath.Join(h.dir, "usage.json")}
	resp = h.post(t, "/v1/chat/completions", hybridKey, chatBody("cloud/unpriced", "hi"))
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "no price") {
		t.Errorf("unpriced: status %d %s", resp.StatusCode, b)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
