// Package ollama serves the Ollama HTTP API on top of an OpenAI API, so
// clients that speak only Ollama work with Sibyl unchanged. The upstream is
// the model router, which checks the caller's key and applies its policy.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Version is the Ollama API version that this shim claims to speak.
const Version = "0.12.0"

// ModelInfo is what the shim needs to know about one installed model.
type ModelInfo struct {
	ID        string
	Size      int64
	Embedding bool
}

// Shim translates Ollama requests to an OpenAI-compatible upstream.
type Shim struct {
	Upstream string                                         // the router, e.g. http://router:8080
	Running  string                                         // llama-swap, for /running; empty means Upstream
	Models   func(ctx context.Context) ([]ModelInfo, error) // installed models
	HTTP     *http.Client                                   // no overall timeout: responses stream
	Now      func() time.Time
}

type authKey struct{}

// withAuth keeps the caller's Authorization header, so the upstream sees
// the caller's key.
func withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), authKey{}, r.Header.Get("Authorization"))))
	}
}

// Register adds the Ollama routes to mux.
func (s *Shim) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/version", s.version)
	mux.HandleFunc("GET /api/tags", s.tags)
	mux.HandleFunc("POST /api/show", s.show)
	mux.HandleFunc("GET /api/ps", s.ps)
	mux.HandleFunc("POST /api/chat", withAuth(s.chat))
	mux.HandleFunc("POST /api/generate", withAuth(s.generate))
	mux.HandleFunc("POST /api/embed", withAuth(s.embed))
	mux.HandleFunc("POST /api/embeddings", withAuth(s.embeddingsLegacy))
	for _, p := range []string{"pull", "push", "create", "copy", "delete", "blobs/"} {
		mux.HandleFunc("/api/"+p, notSupported)
	}
}

func notSupported(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "Sibyl manages models itself: use the Download tab in the TUI or POST /system/downloads",
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Shim) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Shim) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version})
}

type details struct {
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

func ggufDetails() details { return details{Format: "gguf", Families: []string{}} }

type tagModel struct {
	Name       string  `json:"name"`
	Model      string  `json:"model"`
	ModifiedAt string  `json:"modified_at"`
	Size       int64   `json:"size"`
	Digest     string  `json:"digest"`
	Details    details `json:"details"`
}

func (s *Shim) tags(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Models(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := struct {
		Models []tagModel `json:"models"`
	}{Models: []tagModel{}}
	for _, m := range ms {
		out.Models = append(out.Models, tagModel{
			Name: m.ID, Model: m.ID, Size: m.Size, Details: ggufDetails(),
			ModifiedAt: s.now().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Shim) find(ctx context.Context, name string) (ModelInfo, error) {
	ms, err := s.Models(ctx)
	if err != nil {
		return ModelInfo{}, err
	}
	name = strings.TrimSuffix(name, ":latest")
	for _, m := range ms {
		if m.ID == name {
			return m, nil
		}
	}
	return ModelInfo{}, fmt.Errorf("model %q not found", name)
}

func (s *Shim) show(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Model == "" {
		req.Model = req.Name
	}
	m, err := s.find(r.Context(), req.Model)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	caps := []string{"completion", "tools"}
	if m.Embedding {
		caps = []string{"embedding"}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"modelfile": "", "parameters": "", "template": "",
		"details": ggufDetails(), "model_info": map[string]any{}, "capabilities": caps,
		"modified_at": s.now().UTC().Format(time.RFC3339),
	})
}

func (s *Shim) ps(w http.ResponseWriter, r *http.Request) {
	base := s.Running
	if base == "" {
		base = s.Upstream
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/running", nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&running); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type psModel struct {
		Name      string  `json:"name"`
		Model     string  `json:"model"`
		Size      int64   `json:"size"`
		Digest    string  `json:"digest"`
		Details   details `json:"details"`
		ExpiresAt string  `json:"expires_at"`
		SizeVRAM  int64   `json:"size_vram"`
	}
	out := struct {
		Models []psModel `json:"models"`
	}{Models: []psModel{}}
	ms, _ := s.Models(r.Context())
	size := map[string]int64{}
	for _, m := range ms {
		size[m.ID] = m.Size
	}
	for _, m := range running.Running {
		if m.State != "ready" {
			continue
		}
		out.Models = append(out.Models, psModel{
			Name: m.Model, Model: m.Model, Size: size[m.Model], SizeVRAM: size[m.Model],
			Details: ggufDetails(), ExpiresAt: s.now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// options are the Ollama generation options that map to OpenAI fields.
type options struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MinP            *float64 `json:"min_p,omitempty"`
	RepeatPenalty   *float64 `json:"repeat_penalty,omitempty"`
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
	Seed            *int     `json:"seed,omitempty"`
	NumPredict      *int     `json:"num_predict,omitempty"`
	Stop            []string `json:"stop,omitempty"`
}

// toolCall is Ollama's form: arguments are a JSON object.
type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []message       `json:"messages"`
	Stream   *bool           `json:"stream,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  options         `json:"options"`
	Tools    json.RawMessage `json:"tools,omitempty"`
}

// openAIRequest builds the upstream request body.
func openAIRequest(model string, msgs []message, stream bool, o options, format, tools json.RawMessage) (map[string]any, error) {
	var oaMsgs []map[string]any
	for _, m := range msgs {
		om := map[string]any{"role": m.Role}
		if len(m.Images) > 0 {
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + img}})
			}
			om["content"] = parts
		} else {
			om["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			var calls []map[string]any
			for i, tc := range m.ToolCalls {
				args := string(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				calls = append(calls, map[string]any{
					"id": fmt.Sprintf("call_%d", i), "type": "function",
					"function": map[string]string{"name": tc.Function.Name, "arguments": args},
				})
			}
			om["tool_calls"] = calls
		}
		if m.Role == "tool" && m.ToolName != "" {
			om["name"] = m.ToolName
		}
		oaMsgs = append(oaMsgs, om)
	}
	body := map[string]any{"model": model, "messages": oaMsgs, "stream": stream}
	if stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	set := func(k string, v any, ok bool) {
		if ok {
			body[k] = v
		}
	}
	set("temperature", o.Temperature, o.Temperature != nil)
	set("top_p", o.TopP, o.TopP != nil)
	set("top_k", o.TopK, o.TopK != nil)
	set("min_p", o.MinP, o.MinP != nil)
	set("repeat_penalty", o.RepeatPenalty, o.RepeatPenalty != nil)
	set("presence_penalty", o.PresencePenalty, o.PresencePenalty != nil)
	set("seed", o.Seed, o.Seed != nil)
	set("max_tokens", o.NumPredict, o.NumPredict != nil && *o.NumPredict > 0)
	set("stop", o.Stop, len(o.Stop) > 0)
	if len(tools) > 0 && string(tools) != "null" {
		body["tools"] = json.RawMessage(tools)
	}
	switch f := strings.TrimSpace(string(format)); {
	case f == "" || f == "null":
	case f == `"json"`:
		body["response_format"] = map[string]string{"type": "json_object"}
	case strings.HasPrefix(f, "{"):
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "schema": json.RawMessage(format)}}
	default:
		return nil, fmt.Errorf("format must be \"json\" or a JSON schema")
	}
	return body, nil
}

// oaChoice covers both the streamed delta and the final message.
type oaChoice struct {
	Message *oaMessage `json:"message"`
	Delta   *oaMessage `json:"delta"`
	Finish  *string    `json:"finish_reason"`
}

type oaMessage struct {
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content"`
	ToolCalls []struct {
		Index    int `json:"index"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type oaResponse struct {
	Choices []oaChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (s *Shim) upstream(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Upstream+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth, _ := ctx.Value(authKey{}).(string); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &upstreamError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

type upstreamError struct {
	code int
	msg  string
}

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream %d: %s", e.code, e.msg) }

func upstreamStatus(err error) int {
	var ue *upstreamError
	if errors.As(err, &ue) && ue.code >= 400 && ue.code < 500 {
		return ue.code
	}
	return http.StatusBadGateway
}

// ollamaCall converts one OpenAI tool call (arguments as a JSON string) to
// Ollama's form (arguments as a JSON object).
func ollamaCall(name, args string) toolCall {
	var c toolCall
	c.Function.Name = name
	raw := json.RawMessage(args)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(args) // keep malformed arguments as a string
	}
	c.Function.Arguments = raw
	return c
}

// result collects one turn, streamed or not.
type result struct {
	content, thinking string
	calls             map[int]*toolCallAcc
	finish            string
	promptTokens      int
	evalTokens        int
}

type toolCallAcc struct {
	name, args string
}

// toolCalls returns the collected tool calls in index order.
func (r *result) toolCalls() []toolCall {
	var out []toolCall
	for i := 0; len(out) < len(r.calls) && i < 1024; i++ {
		if c, ok := r.calls[i]; ok {
			out = append(out, ollamaCall(c.name, c.args))
		}
	}
	return out
}

// run sends the request and calls emit for each piece of text as it arrives.
// It returns the whole turn.
func (s *Shim) run(ctx context.Context, body map[string]any, stream bool, emit func(content, thinking string) error) (*result, error) {
	resp, err := s.upstream(ctx, "/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	res := &result{calls: map[int]*toolCallAcc{}, finish: "stop"}
	if !stream {
		var r oaResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
		if len(r.Choices) > 0 && r.Choices[0].Message != nil {
			m := r.Choices[0].Message
			res.content, res.thinking = m.Content, m.Reasoning
			for i, tc := range m.ToolCalls {
				res.calls[i] = &toolCallAcc{tc.Function.Name, tc.Function.Arguments}
			}
			if f := r.Choices[0].Finish; f != nil {
				res.finish = *f
			}
		}
		if r.Usage != nil {
			res.promptTokens, res.evalTokens = r.Usage.PromptTokens, r.Usage.CompletionTokens
		}
		return res, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "[DONE]" {
			break
		}
		var chunk oaResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return res, fmt.Errorf("bad stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			res.promptTokens, res.evalTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.Finish != nil && *c.Finish != "" {
				res.finish = *c.Finish
			}
			if c.Delta == nil {
				continue
			}
			for _, tc := range c.Delta.ToolCalls {
				acc, ok := res.calls[tc.Index]
				if !ok {
					acc = &toolCallAcc{}
					res.calls[tc.Index] = acc
				}
				acc.name += tc.Function.Name
				acc.args += tc.Function.Arguments
			}
			if c.Delta.Content == "" && c.Delta.Reasoning == "" {
				continue
			}
			res.content += c.Delta.Content
			res.thinking += c.Delta.Reasoning
			if err := emit(c.Delta.Content, c.Delta.Reasoning); err != nil {
				return res, err
			}
		}
	}
	return res, sc.Err()
}

func streamOn(p *bool) bool { return p == nil || *p }

// noDeadline lifts the server write timeout for a streamed response.
func noDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func (s *Shim) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.turn(w, r, req.Model, req.Messages, streamOn(req.Stream), req.Options, req.Format, req.Tools, false)
}

func (s *Shim) generate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string          `json:"model"`
		Prompt  string          `json:"prompt"`
		System  string          `json:"system"`
		Images  []string        `json:"images"`
		Stream  *bool           `json:"stream"`
		Format  json.RawMessage `json:"format"`
		Options options         `json:"options"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Prompt == "" {
		// Ollama clients load a model with an empty prompt; nothing to generate.
		writeJSON(w, http.StatusOK, map[string]any{"model": req.Model, "created_at": s.now().UTC().Format(time.RFC3339Nano), "response": "", "done": true, "done_reason": "load"})
		return
	}
	var msgs []message
	if req.System != "" {
		msgs = append(msgs, message{Role: "system", Content: req.System})
	}
	msgs = append(msgs, message{Role: "user", Content: req.Prompt, Images: req.Images})
	s.turn(w, r, req.Model, msgs, streamOn(req.Stream), req.Options, req.Format, nil, true)
}

// turn runs one chat turn and writes it in Ollama's chat or generate shape.
func (s *Shim) turn(w http.ResponseWriter, r *http.Request, model string, msgs []message, stream bool, o options, format, tools json.RawMessage, generate bool) {
	if _, err := s.find(r.Context(), model); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	model = strings.TrimSuffix(model, ":latest")
	body, err := openAIRequest(model, msgs, stream, o, format, tools)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	start := s.now()
	created := func() string { return s.now().UTC().Format(time.RFC3339Nano) }
	frame := func(content, thinking string, done bool) map[string]any {
		f := map[string]any{"model": model, "created_at": created(), "done": done}
		if generate {
			f["response"] = content
			if thinking != "" {
				f["thinking"] = thinking
			}
		} else {
			msg := map[string]any{"role": "assistant", "content": content}
			if thinking != "" {
				msg["thinking"] = thinking
			}
			f["message"] = msg
		}
		return f
	}
	final := func(res *result) map[string]any {
		f := frame("", "", true)
		if !stream {
			f = frame(res.content, res.thinking, true)
		}
		if calls := res.toolCalls(); len(calls) > 0 && !generate {
			f["message"].(map[string]any)["tool_calls"] = calls
		}
		f["done_reason"] = res.finish
		f["total_duration"] = s.now().Sub(start).Nanoseconds()
		f["prompt_eval_count"] = res.promptTokens
		f["eval_count"] = res.evalTokens
		return f
	}

	if !stream {
		res, err := s.run(r.Context(), body, false, nil)
		if err != nil {
			writeErr(w, upstreamStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, final(res))
		return
	}

	noDeadline(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	started := false
	res, err := s.run(r.Context(), body, true, func(content, thinking string) error {
		started = true
		if err := enc.Encode(frame(content, thinking, false)); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		if !started {
			writeErr(w, upstreamStatus(err), err)
			return
		}
		_ = enc.Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = enc.Encode(final(res))
}

func (s *Shim) embedUpstream(ctx context.Context, model string, input any) ([][]float64, int, error) {
	resp, err := s.upstream(ctx, "/v1/embeddings", map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, 0, err
	}
	out := make([][]float64, len(r.Data))
	for _, d := range r.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, r.Usage.PromptTokens, nil
}

func (s *Shim) embed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.find(r.Context(), req.Model); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var input any
	if err := json.Unmarshal(req.Input, &input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("input must be a string or a list of strings"))
		return
	}
	vecs, tokens, err := s.embedUpstream(r.Context(), strings.TrimSuffix(req.Model, ":latest"), input)
	if err != nil {
		writeErr(w, upstreamStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": req.Model, "embeddings": vecs, "prompt_eval_count": tokens})
}

func (s *Shim) embeddingsLegacy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.find(r.Context(), req.Model); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	vecs, _, err := s.embedUpstream(r.Context(), strings.TrimSuffix(req.Model, ":latest"), req.Prompt)
	if err != nil {
		writeErr(w, upstreamStatus(err), err)
		return
	}
	var v []float64
	if len(vecs) > 0 {
		v = vecs[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"embedding": v})
}
