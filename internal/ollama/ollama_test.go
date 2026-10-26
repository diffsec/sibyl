package ollama

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeUpstream is a minimal llama-swap: OpenAI chat (streamed or not),
// embeddings, and /running. It records the last chat request body.
type fakeUpstream struct {
	lastChat map[string]any
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/running":
		_, _ = io.WriteString(w, `{"running":[{"model":"chat","state":"ready"},{"model":"embed","state":"starting"}]}`)
	case "/v1/embeddings":
		_, _ = io.WriteString(w, `{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":5}}`)
	case "/v1/chat/completions":
		f.lastChat = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.lastChat)
		if f.lastChat["model"] == "broken" {
			http.Error(w, `{"error":"model failed to load"}`, http.StatusBadRequest)
			return
		}
		if f.lastChat["stream"] != true {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello there","reasoning_content":"think","tool_calls":[{"function":{"name":"get_weather","arguments":"{\"city\":\"Hobart\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`,
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func newShim(t *testing.T) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	s := &Shim{
		Upstream: upSrv.URL,
		HTTP:     &http.Client{},
		Now:      func() time.Time { return time.Unix(1_800_000_000, 0) },
		Models: func(context.Context) ([]ModelInfo, error) {
			return []ModelInfo{{ID: "chat", Size: 100}, {ID: "embed", Size: 10, Embedding: true}, {ID: "broken"}}, nil
		},
	}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, up
}

func post(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestChatNonStreamWithTools(t *testing.T) {
	srv, up := newShim(t)
	code, body := post(t, srv, "/api/chat", `{"model":"chat:latest","stream":false,
		"messages":[{"role":"user","content":"hi"},
		            {"role":"assistant","content":"","tool_calls":[{"function":{"name":"f","arguments":{"a":1}}}]},
		            {"role":"tool","content":"42","tool_name":"f"}],
		"options":{"temperature":0.2,"top_k":5,"num_predict":64,"stop":["END"]},
		"format":"json"}`)
	if code != 200 {
		t.Fatalf("code %d: %s", code, body)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(body), &r)
	msg := r["message"].(map[string]any)
	if msg["content"] != "Hello there" || msg["thinking"] != "think" || r["done"] != true || r["done_reason"] != "tool_calls" {
		t.Errorf("response = %s", body)
	}
	calls := msg["tool_calls"].([]any)
	args := calls[0].(map[string]any)["function"].(map[string]any)["arguments"].(map[string]any)
	if args["city"] != "Hobart" {
		t.Errorf("tool call arguments = %v", args)
	}
	if r["prompt_eval_count"].(float64) != 7 || r["eval_count"].(float64) != 3 {
		t.Errorf("token counts = %v %v", r["prompt_eval_count"], r["eval_count"])
	}

	// The upstream request: model without :latest, options mapped, tool call
	// arguments turned into a string, tool name kept, JSON mode on.
	sent := up.lastChat
	if sent["model"] != "chat" || sent["temperature"] != 0.2 || sent["top_k"] != 5.0 || sent["max_tokens"] != 64.0 {
		t.Errorf("upstream body = %v", sent)
	}
	if sent["response_format"].(map[string]any)["type"] != "json_object" {
		t.Errorf("response_format = %v", sent["response_format"])
	}
	msgs := sent["messages"].([]any)
	tc := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tc["arguments"] != `{"a":1}` {
		t.Errorf("assistant tool call arguments = %v", tc["arguments"])
	}
	if msgs[2].(map[string]any)["name"] != "f" {
		t.Errorf("tool message = %v", msgs[2])
	}
}

func TestChatStream(t *testing.T) {
	srv, up := newShim(t)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type = %s", ct)
	}
	var frames []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		frames = append(frames, f)
	}
	if len(frames) != 4 {
		t.Fatalf("frames = %v", frames)
	}
	text := ""
	for _, f := range frames[:3] {
		text += f["message"].(map[string]any)["content"].(string)
	}
	if text != "Hello" || frames[0]["message"].(map[string]any)["thinking"] != "hmm" {
		t.Errorf("streamed text %q, first frame %v", text, frames[0])
	}
	last := frames[3]
	if last["done"] != true || last["done_reason"] != "stop" || last["eval_count"].(float64) != 2 {
		t.Errorf("final frame = %v", last)
	}
	if up.lastChat["stream"] != true || up.lastChat["stream_options"] == nil {
		t.Errorf("upstream stream flags = %v", up.lastChat)
	}
}

func TestGenerate(t *testing.T) {
	srv, up := newShim(t)
	code, body := post(t, srv, "/api/generate", `{"model":"chat","prompt":"hi","system":"be brief","stream":false}`)
	if code != 200 || !strings.Contains(body, `"response":"Hello there"`) {
		t.Fatalf("code %d: %s", code, body)
	}
	msgs := up.lastChat["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("messages = %v", msgs)
	}
	// An empty prompt only loads the model.
	code, body = post(t, srv, "/api/generate", `{"model":"chat"}`)
	if code != 200 || !strings.Contains(body, `"done_reason":"load"`) {
		t.Errorf("load-only generate = %d %s", code, body)
	}
}

func TestEmbed(t *testing.T) {
	srv, _ := newShim(t)
	code, body := post(t, srv, "/api/embed", `{"model":"embed","input":["a","b"]}`)
	if code != 200 || !strings.Contains(body, `"embeddings":[[0.1,0.2],[0.3,0.4]]`) {
		t.Errorf("embed = %d %s", code, body)
	}
	code, body = post(t, srv, "/api/embeddings", `{"model":"embed","prompt":"a"}`)
	if code != 200 || !strings.Contains(body, `"embedding":[0.1,0.2]`) {
		t.Errorf("legacy embeddings = %d %s", code, body)
	}
}

func TestTagsShowPsAndErrors(t *testing.T) {
	srv, _ := newShim(t)
	resp, err := http.Get(srv.URL + "/api/tags")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), `"name":"chat"`) || !strings.Contains(string(b), `"format":"gguf"`) {
		t.Errorf("tags = %s", b)
	}
	code, body := post(t, srv, "/api/show", `{"name":"embed"}`)
	if code != 200 || !strings.Contains(body, `"capabilities":["embedding"]`) {
		t.Errorf("show = %d %s", code, body)
	}
	if code, _ := post(t, srv, "/api/show", `{"model":"nope"}`); code != 404 {
		t.Errorf("show unknown = %d", code)
	}
	resp, err = http.Get(srv.URL + "/api/ps")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), `"name":"chat"`) || strings.Contains(string(b), `"name":"embed"`) {
		t.Errorf("ps should list only ready models: %s", b)
	}
	if code, body := post(t, srv, "/api/chat", `{"model":"broken","stream":false,"messages":[]}`); code != 400 || !strings.Contains(body, "model failed to load") {
		t.Errorf("upstream 400 = %d %s", code, body)
	}
	if code, _ := post(t, srv, "/api/chat", `{"model":"missing","messages":[]}`); code != 404 {
		t.Errorf("unknown model = %d", code)
	}
	if code, _ := post(t, srv, "/api/pull", `{"model":"x"}`); code != 501 {
		t.Errorf("pull = %d", code)
	}
}
