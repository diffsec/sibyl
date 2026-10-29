package router

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The Anthropic Messages API on the client side. The router reads a
// Messages request into an OpenAI chat request, routes it like any chat
// request, and writes the chat response back in the Messages format.

type msgRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`
	Messages      []msgMessage    `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Tools         []msgTool       `json:"tools"`
	ToolChoice    *msgToolChoice  `json:"tool_choice"`
}

type msgMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type msgBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type msgTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type msgToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

func parseMessages(body []byte) (msgRequest, error) {
	var r msgRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return r, errors.New("the request body is not valid JSON for a Messages request")
	}
	if r.Model == "" {
		return r, errors.New("model is required")
	}
	if r.MaxTokens <= 0 {
		return r, errors.New("max_tokens is required")
	}
	return r, nil
}

// blocks reads content that is a string or a list of blocks.
func blocks(raw json.RawMessage) []msgBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []msgBlock{{Type: "text", Text: s}}
	}
	var bs []msgBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// blockText joins the text blocks of content.
func blockText(raw json.RawMessage) string {
	var parts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toChat translates a Messages request into an OpenAI chat request. local
// adds top_k, which llama-server accepts and the OpenAI API refuses.
func (r msgRequest) toChat(model string, local bool) map[string]any {
	var msgs []map[string]any
	if sys := blockText(r.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			msgs = append(msgs, assistantToChat(m.Content))
			continue
		}
		var parts []map[string]any
		textOnly := true
		for _, b := range blocks(m.Content) {
			switch b.Type {
			case "tool_result":
				content := blockText(b.Content)
				if b.IsError {
					content = "Error: " + content
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			case "image":
				if b.Source == nil {
					continue
				}
				url := b.Source.URL
				if b.Source.Type == "base64" {
					url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
				textOnly = false
			}
		}
		switch {
		case len(parts) == 0:
		case textOnly:
			var texts []string
			for _, p := range parts {
				texts = append(texts, p["text"].(string))
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
		default:
			msgs = append(msgs, map[string]any{"role": "user", "content": parts})
		}
	}

	out := map[string]any{"model": model, "messages": msgs, "max_tokens": r.MaxTokens}
	if len(r.StopSequences) > 0 {
		out["stop"] = r.StopSequences
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if r.TopK != nil && local {
		out["top_k"] = *r.TopK
	}
	if r.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		// Server tools, such as web search, have no input schema. They run
		// only on Anthropic's servers.
		if len(t.InputSchema) == 0 {
			continue
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
		}})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if tc := r.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto", "none":
				out["tool_choice"] = tc.Type
			case "any":
				out["tool_choice"] = "required"
			case "tool":
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				out["parallel_tool_calls"] = false
			}
		}
	}
	return out
}

func assistantToChat(content json.RawMessage) map[string]any {
	var texts []string
	var calls []map[string]any
	for _, b := range blocks(content) {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{"id": b.ID, "type": "function",
				"function": map[string]string{"name": b.Name, "arguments": args}})
		}
	}
	m := map[string]any{"role": "assistant"}
	if len(texts) > 0 {
		m["content"] = strings.Join(texts, "\n")
	} else {
		m["content"] = nil
	}
	if len(calls) > 0 {
		m["tool_calls"] = calls
	}
	return m
}

// features computes the profile of a Messages request from its chat form.
func (r msgRequest) features(words []string) Features {
	body, _ := json.Marshal(r.toChat(r.Model, true))
	c, _ := parseChat(body)
	return c.features(words)
}

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

var stopReasons = map[string]string{
	"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
	"function_call": "tool_use", "content_filter": "refusal",
}

func stopReason(finish string) string {
	if s, ok := stopReasons[finish]; ok {
		return s
	}
	return "end_turn"
}

type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

func msgUsage(u chatUsage) map[string]int {
	return map[string]int{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens}
}

// toMessage translates a chat response body into a Messages response.
func toMessage(body []byte, model string) (map[string]any, chatUsage, error) {
	var c chatResponse
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, chatUsage{}, fmt.Errorf("read the upstream response: %w", err)
	}
	content := []map[string]any{}
	finish := "stop"
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		finish = ch.FinishReason
		if ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			content = append(content, toolUse(tc.ID, tc.Function.Name, tc.Function.Arguments))
		}
	}
	return map[string]any{
		"id": newID("msg_"), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason(finish), "stop_sequence": nil,
		"usage": msgUsage(c.Usage),
	}, c.Usage, nil
}

func toolUse(id, name, args string) map[string]any {
	if id == "" {
		id = newID("toolu_")
	}
	input := json.RawMessage(args)
	if !json.Valid(input) {
		input = json.RawMessage("{}")
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// msgError writes an error in the Messages format.
func msgError(w http.ResponseWriter, code int, msg string) {
	typ := "api_error"
	switch code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		typ = "invalid_request_error"
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusForbidden:
		typ = "permission_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusServiceUnavailable:
		typ = "overloaded_error"
	}
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}

// upstreamMessage returns the error message of an OpenAI-style error body.
func upstreamMessage(body []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Error) > 0 {
		var s string
		if json.Unmarshal(e.Error, &s) == nil {
			return s
		}
		var o struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &o) == nil && o.Message != "" {
			return o.Message
		}
	}
	return strings.TrimSpace(string(body))
}

// streamMessages reads an OpenAI chat stream and writes Messages API events.
// It returns the usage of the last chunk that held one.
func streamMessages(w http.ResponseWriter, upstream io.Reader, model string) (chatUsage, error) {
	flusher, _ := w.(http.Flusher)
	var werr error
	event := func(name string, data any) {
		if werr != nil {
			return
		}
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			werr = err
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": newID("msg_"), "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})

	index := -1    // the open content block, or -1
	openKind := "" // "text" or "tool"
	toolIdx := -1  // the chat tool call index of the open tool block
	closeBlock := func() {
		if index >= 0 && openKind != "" {
			event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			openKind = ""
		}
	}
	finish := "stop"
	var usage chatUsage

	sc := bufio.NewScanner(upstream)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   string         `json:"content"`
					ToolCalls []chatToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *chatUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			usage = *ch.Usage
		}
		if len(ch.Choices) == 0 {
			continue
		}
		c := ch.Choices[0]
		if c.Delta.Content != "" {
			if openKind != "text" {
				closeBlock()
				index++
				openKind = "text"
				event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
					"content_block": map[string]string{"type": "text", "text": ""}})
			}
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]string{"type": "text_delta", "text": c.Delta.Content}})
		}
		for _, tc := range c.Delta.ToolCalls {
			if openKind != "tool" || tc.Index != toolIdx {
				closeBlock()
				index++
				openKind, toolIdx = "tool", tc.Index
				id := tc.ID
				if id == "" {
					id = newID("toolu_")
				}
				event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{}}})
			}
			if tc.Function.Arguments != "" {
				event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			finish = *c.FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		event("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": err.Error()}})
		return usage, err
	}
	closeBlock()
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(finish), "stop_sequence": nil},
		"usage": msgUsage(usage)})
	event("message_stop", map[string]string{"type": "message_stop"})
	return usage, werr
}
