package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The Anthropic adapter: OpenAI chat requests to a provider of the type
// anthropic. It translates the request into the Messages API with the
// official Go SDK, and the response back into the chat format, so the rest
// of the router sees an OpenAI-compatible provider.

// Default output limits, which the Messages API requires and the chat
// format leaves optional.
const (
	defaultMaxTokens       = 16000
	defaultMaxTokensStream = 64000
)

// anthropicChat is the part of an OpenAI chat request that the adapter
// translates. Fields it does not know are named in X-Sibyl-Dropped.
type anthropicChat struct {
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// translated is the request fields that the adapter uses. Every other
// top-level field of a chat request is dropped.
var translated = []string{
	"model", "messages", "tools", "tool_choice", "parallel_tool_calls", "max_tokens",
	"max_completion_tokens", "stop", "stream", "stream_options", "reasoning_effort", "user",
}

// dropped lists the top-level fields of body that the adapter does not send.
// temperature, top_p and top_k are among them: current Claude models accept
// only their defaults.
func dropped(body []byte) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var out []string
	for k, v := range m {
		if !slices.Contains(translated, k) && string(v) != "null" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// textParts reads chat content: a string, or a list of parts.
func textParts(raw json.RawMessage) (texts []string, images []string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	_ = json.Unmarshal(raw, &parts)
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url":
			images = append(images, p.ImageURL.URL)
		}
	}
	return texts, images
}

func imageBlock(url string) anthropic.ContentBlockParamUnion {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if mt, data, ok := strings.Cut(rest, ";base64,"); ok {
			return anthropic.ContentBlockParamUnion{OfImage: &anthropic.ImageBlockParam{
				Source: anthropic.ImageBlockParamSourceUnion{OfBase64: &anthropic.Base64ImageSourceParam{
					Data: data, MediaType: anthropic.Base64ImageSourceMediaType(mt),
				}},
			}}
		}
	}
	return anthropic.ContentBlockParamUnion{OfImage: &anthropic.ImageBlockParam{
		Source: anthropic.ImageBlockParamSourceUnion{OfURL: &anthropic.URLImageSourceParam{URL: url}},
	}}
}

// toolSchema turns a JSON Schema object into the SDK's input schema.
func toolSchema(raw json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	var m map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &m); err != nil {
			return anthropic.ToolInputSchemaParam{}, err
		}
	}
	s := anthropic.ToolInputSchemaParam{Properties: map[string]any{}}
	for k, v := range m {
		switch k {
		case "type":
		case "properties":
			s.Properties = v
		case "required":
			for _, r := range asList(v) {
				if name, ok := r.(string); ok {
					s.Required = append(s.Required, name)
				}
			}
		default:
			if s.ExtraFields == nil {
				s.ExtraFields = map[string]any{}
			}
			s.ExtraFields[k] = v
		}
	}
	return s, nil
}

func asList(v any) []any { l, _ := v.([]any); return l }

// toAnthropic translates an OpenAI chat request body.
func toAnthropic(body []byte, model string) (anthropic.MessageNewParams, anthropicChat, error) {
	var c anthropicChat
	if err := json.Unmarshal(body, &c); err != nil {
		return anthropic.MessageNewParams{}, c, errors.New("the request body is not valid JSON for a chat request")
	}
	p := anthropic.MessageNewParams{
		Model: anthropic.Model(model),
		// Automatic prompt caching: the API places the breakpoint on the
		// last cacheable block, so the next turn reads the conversation
		// from the cache.
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	switch {
	case c.MaxCompletionTokens != nil:
		p.MaxTokens = *c.MaxCompletionTokens
	case c.MaxTokens != nil:
		p.MaxTokens = *c.MaxTokens
	case c.Stream:
		p.MaxTokens = defaultMaxTokensStream
	default:
		p.MaxTokens = defaultMaxTokens
	}

	add := func(role anthropic.MessageParamRole, blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		// The Messages API wants the roles to alternate, so turns of the
		// same role merge, such as several tool results.
		if n := len(p.Messages); n > 0 && p.Messages[n-1].Role == role {
			p.Messages[n-1].Content = append(p.Messages[n-1].Content, blocks...)
			return
		}
		p.Messages = append(p.Messages, anthropic.MessageParam{Role: role, Content: blocks})
	}
	for _, m := range c.Messages {
		texts, images := textParts(m.Content)
		var blocks []anthropic.ContentBlockParamUnion
		for _, t := range texts {
			if strings.TrimSpace(t) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(t))
			}
		}
		switch m.Role {
		case "system", "developer":
			for _, t := range texts {
				if strings.TrimSpace(t) != "" {
					p.System = append(p.System, anthropic.TextBlockParam{Text: t})
				}
			}
		case "user":
			for _, u := range images {
				blocks = append(blocks, imageBlock(u))
			}
			add(anthropic.MessageParamRoleUser, blocks...)
		case "assistant":
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
			}
			add(anthropic.MessageParamRoleAssistant, blocks...)
		case "tool":
			add(anthropic.MessageParamRoleUser, anthropic.NewToolResultBlock(m.ToolCallID, strings.Join(texts, "\n"), false))
		default:
			return p, c, fmt.Errorf("message role %q is not supported", m.Role)
		}
	}
	if len(p.Messages) == 0 {
		return p, c, errors.New("the request has no user or assistant message")
	}

	for _, t := range c.Tools {
		schema, err := toolSchema(t.Function.Parameters)
		if err != nil {
			return p, c, fmt.Errorf("tool %q: %w", t.Function.Name, err)
		}
		tool := anthropic.ToolParam{Name: t.Function.Name, InputSchema: schema}
		if t.Function.Description != "" {
			tool.Description = anthropic.String(t.Function.Description)
		}
		p.Tools = append(p.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	noParallel := c.ParallelToolCalls != nil && !*c.ParallelToolCalls
	var choice string
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(c.ToolChoice, &choice) != nil && json.Unmarshal(c.ToolChoice, &named) == nil && named.Function.Name != "" {
		choice = "function"
	}
	switch {
	case len(p.Tools) == 0:
	case choice == "none":
		none := anthropic.NewToolChoiceNoneParam()
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &none}
	case choice == "required":
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case choice == "function":
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: named.Function.Name}}
	case noParallel:
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
	if noParallel {
		switch {
		case p.ToolChoice.OfAuto != nil:
			p.ToolChoice.OfAuto.DisableParallelToolUse = anthropic.Bool(true)
		case p.ToolChoice.OfAny != nil:
			p.ToolChoice.OfAny.DisableParallelToolUse = anthropic.Bool(true)
		case p.ToolChoice.OfTool != nil:
			p.ToolChoice.OfTool.DisableParallelToolUse = anthropic.Bool(true)
		}
	}

	var stops []string
	if json.Unmarshal(c.Stop, &stops) != nil {
		var one string
		if json.Unmarshal(c.Stop, &one) == nil && one != "" {
			stops = []string{one}
		}
	}
	p.StopSequences = stops

	switch e := strings.ToLower(c.ReasoningEffort); e {
	case "":
	case "minimal", "none":
		p.OutputConfig.Effort = anthropic.OutputConfigEffortLow
	default:
		p.OutputConfig.Effort = anthropic.OutputConfigEffort(e)
	}
	return p, c, nil
}

var finishReasons = map[anthropic.StopReason]string{
	anthropic.StopReasonEndTurn:                    "stop",
	anthropic.StopReasonStopSequence:               "stop",
	anthropic.StopReasonPauseTurn:                  "stop",
	anthropic.StopReasonMaxTokens:                  "length",
	anthropic.StopReasonModelContextWindowExceeded: "length",
	anthropic.StopReasonToolUse:                    "tool_calls",
	anthropic.StopReasonRefusal:                    "content_filter",
}

func finishReason(r anthropic.StopReason) string {
	if f, ok := finishReasons[r]; ok {
		return f
	}
	return "stop"
}

// chatUsageOf writes Anthropic usage in the chat format. prompt_tokens
// counts cached tokens too, as the OpenAI API does.
func chatUsageOf(in, out, read, write int64) map[string]any {
	prompt := in + read + write
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": out, "total_tokens": prompt + out,
		"prompt_tokens_details":       map[string]int64{"cached_tokens": read},
		"cache_creation_input_tokens": write,
	}
}

// fromMessage translates a Messages response into a chat completion.
func fromMessage(m *anthropic.Message) map[string]any {
	msg := map[string]any{"role": "assistant"}
	var text, thinking strings.Builder
	var calls []map[string]any
	for _, b := range m.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(v.Text)
		case anthropic.ThinkingBlock:
			thinking.WriteString(v.Thinking)
		case anthropic.ToolUseBlock:
			args := string(v.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{"id": v.ID, "type": "function",
				"function": map[string]string{"name": v.Name, "arguments": args}})
		}
	}
	msg["content"] = text.String()
	if thinking.Len() > 0 {
		msg["reasoning_content"] = thinking.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	u := m.Usage
	return map[string]any{
		"id": m.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": string(m.Model),
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finishReason(m.StopReason)}},
		"usage":   chatUsageOf(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens),
	}
}

// anthropicClient returns an SDK client for a provider. The router retries
// and falls back itself, so the SDK does not retry.
func (s *Server) anthropicClient(p Provider, key string) anthropic.Client {
	return anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(p.URL()+"/"),
		option.WithAPIKey(key),
		option.WithHTTPClient(s.HTTP),
		option.WithMaxRetries(0),
	)
}

// errorResponse turns an SDK error into an OpenAI-style error response, so
// the fallback logic sees the provider's status code.
func errorResponse(err error) *http.Response {
	code, msg := http.StatusBadGateway, err.Error()
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		code = ae.StatusCode
		msg = upstreamMessage([]byte(ae.RawJSON()))
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "provider_error", "code": code}})
	h := http.Header{"Content-Type": {"application/json"}}
	if ae != nil && ae.Response != nil {
		if ra := ae.Response.Header.Get("Retry-After"); ra != "" {
			h.Set("Retry-After", ra)
		}
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(bytes.NewReader(b))}
}

// anthropicChatCall sends a chat request to an anthropic provider and
// returns a response in the chat format.
func (s *Server) anthropicChatCall(ctx context.Context, cfg *Config, rt Route, body []byte) (*http.Response, error) {
	params, chat, err := toAnthropic(body, rt.Model)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "%v", err)
	}
	client := s.anthropicClient(cfg.Providers.Providers[rt.Provider], cfg.Keys[rt.Provider])
	header := http.Header{}
	if d := dropped(body); len(d) > 0 {
		header.Set("X-Sibyl-Dropped", strings.Join(d, ","))
	}
	if !chat.Stream {
		m, err := client.Messages.New(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			return errorResponse(err), nil
		}
		b, _ := json.Marshal(fromMessage(m))
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}

	stream := client.Messages.NewStreaming(ctx, params)
	// The first event shows whether the request was accepted, so an error
	// still reaches the fallback logic before any byte goes to the client.
	if !stream.Next() {
		err := stream.Err()
		_ = stream.Close()
		if err == nil {
			err = errors.New("the provider closed the stream before the first event")
		}
		if ctx.Err() != nil {
			return nil, err
		}
		return errorResponse(err), nil
	}
	pr, pw := io.Pipe()
	includeUsage := chat.StreamOptions != nil && chat.StreamOptions.IncludeUsage
	go func() {
		defer func() { _ = stream.Close() }()
		pw.CloseWithError(writeChatStream(pw, stream.Current(), stream.Next, stream.Current, stream.Err, includeUsage))
	}()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: pr}, nil
}

// writeChatStream writes Messages stream events as chat completion chunks,
// starting with first. The usage goes into its own chunk when the client
// asked for it, and into the last chunk otherwise, so the router can count
// the cost either way.
func writeChatStream(w io.Writer, first anthropic.MessageStreamEventUnion, next func() bool,
	current func() anthropic.MessageStreamEventUnion, streamErr func() error, includeUsage bool) error {
	var id, model string
	var in, out, read, write int64
	created := time.Now().Unix()
	toolIndex := map[int64]int{} // content block index -> tool call index
	finish := "stop"
	chunk := func(delta map[string]any, finishReason any, usage any) error {
		c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}}}
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		_, err := fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}

	ev := first
	for {
		switch e := ev.AsAny().(type) {
		case anthropic.MessageStartEvent:
			id, model = e.Message.ID, string(e.Message.Model)
			u := e.Message.Usage
			in, read, write = u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens
			if err := chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil); err != nil {
				return err
			}
		case anthropic.ContentBlockStartEvent:
			if e.ContentBlock.Type == "tool_use" {
				i := len(toolIndex)
				toolIndex[e.Index] = i
				if err := chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": e.ContentBlock.ID,
					"type": "function", "function": map[string]string{"name": e.ContentBlock.Name, "arguments": ""}}}}, nil, nil); err != nil {
					return err
				}
			}
		case anthropic.ContentBlockDeltaEvent:
			var delta map[string]any
			switch d := e.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				delta = map[string]any{"content": d.Text}
			case anthropic.ThinkingDelta:
				delta = map[string]any{"reasoning_content": d.Thinking}
			case anthropic.InputJSONDelta:
				if i, ok := toolIndex[e.Index]; ok && d.PartialJSON != "" {
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": i,
						"function": map[string]string{"arguments": d.PartialJSON}}}}
				}
			}
			if delta != nil {
				if err := chunk(delta, nil, nil); err != nil {
					return err
				}
			}
		case anthropic.MessageDeltaEvent:
			finish = finishReason(e.Delta.StopReason)
			u := e.Usage
			out = max(out, u.OutputTokens)
			in, read, write = max(in, u.InputTokens), max(read, u.CacheReadInputTokens), max(write, u.CacheCreationInputTokens)
		}
		if !next() {
			break
		}
		ev = current()
	}
	if err := streamErr(); err != nil {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "provider_error"}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	usage := chatUsageOf(in, out, read, write)
	if includeUsage {
		if err := chunk(map[string]any{}, finish, nil); err != nil {
			return err
		}
		c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{}, "usage": usage}
		b, _ := json.Marshal(c)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
	} else if err := chunk(map[string]any{}, finish, usage); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// anthropicPassthrough sends a Messages request to an anthropic provider
// unchanged, apart from the model name and the key. The client's API
// version and beta headers go with it.
func (s *Server) anthropicPassthrough(ctx context.Context, cfg *Config, rt Route, body []byte, in http.Header) (*http.Response, error) {
	b, err := withModel(body, rt.Model)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Providers.Providers[rt.Provider].URL()+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", cfg.Keys[rt.Provider])
	version := in.Get("Anthropic-Version")
	if version == "" {
		version = "2023-06-01"
	}
	req.Header.Set("Anthropic-Version", version)
	if beta := in.Values("Anthropic-Beta"); len(beta) > 0 {
		req.Header["Anthropic-Beta"] = beta
	}
	return s.HTTP.Do(req)
}
