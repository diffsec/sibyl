package router

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// chatRequest holds the fields of an OpenAI chat completions request that
// the router reads. The router forwards the original body, not this struct.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               json.RawMessage `json:"tools"`
	Stream              bool            `json:"stream"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	ResponseFormat      *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type chatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseChat(body []byte) (chatRequest, error) {
	var r chatRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return r, errors.New("the request body is not valid JSON for a chat request")
	}
	if r.Model == "" {
		return r, errors.New("model is required")
	}
	return r, nil
}

// Features is the profile of a request that the rules read. Only local work
// makes it: no content leaves the host to make it.
type Features struct {
	Client       string `json:"client"`
	Policy       Policy `json:"policy"`
	PromptTokens int    `json:"prompt_tokens"`
	MaxTokens    int    `json:"max_tokens"`
	Stream       bool   `json:"stream"`
	HasImages    bool   `json:"has_images"`
	HasTools     bool   `json:"has_tools"`
	JSONSchema   bool   `json:"json_schema"`
	LocalContext int    `json:"local_context"`
	LocalReady   bool   `json:"local_ready"`
	Sensitive    bool   `json:"sensitive"`
	Task         string `json:"task"`
	Difficulty   string `json:"difficulty"`
	Hint         string `json:"hint"`
	// NeedsProvider is the calibration vote. Nil means unknown.
	NeedsProvider *bool `json:"needs_provider,omitempty"`
	// query is the text that the classifier labels: the last user message.
	query string
}

// charsPerToken is the estimate of characters per token. English text runs
// near 4 characters per token, so 3 counts long rather than short: a request
// near the local context goes to the larger context, not to a failure.
const charsPerToken = 3

// text returns the text of a request that counts toward its tokens, and
// whether the request holds an image.
func (r chatRequest) text() (string, bool) {
	var b strings.Builder
	images := false
	for _, m := range r.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			b.WriteString(s)
		} else {
			var parts []contentPart
			_ = json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				switch p.Type {
				case "text":
					b.WriteString(p.Text)
				case "image_url", "input_image":
					images = true
				}
			}
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(tc.Function.Name)
			b.WriteString(tc.Function.Arguments)
		}
		b.WriteByte('\n')
	}
	b.Write(r.Tools)
	return b.String(), images
}

// messageText returns the text of a message's content.
func messageText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []contentPart
	_ = json.Unmarshal(content, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// lastUser returns the text of the last user message that has text.
func (r chatRequest) lastUser() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			if t := messageText(r.Messages[i].Content); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// features computes the parts of the profile that come from the request.
func (r chatRequest) features(words []string) Features {
	text, images := r.text()
	f := Features{
		PromptTokens: (len(text) + charsPerToken - 1) / charsPerToken,
		Stream:       r.Stream,
		HasImages:    images,
		HasTools:     len(r.Tools) > 0 && string(r.Tools) != "null" && string(r.Tools) != "[]",
		JSONSchema:   r.ResponseFormat != nil && (r.ResponseFormat.Type == "json_schema" || r.ResponseFormat.Type == "json_object"),
		Sensitive:    Sensitive(text, words),
		Task:         "unknown",
		Difficulty:   "unknown",
		query:        r.lastUser(),
	}
	switch {
	case r.MaxCompletionTokens != nil:
		f.MaxTokens = *r.MaxCompletionTokens
	case r.MaxTokens != nil:
		f.MaxTokens = *r.MaxTokens
	}
	return f
}

// secretPatterns find credentials in request text. A match keeps the
// request on the host when a rule tests "sensitive".
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                             // AWS access key ID
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),                   // GitHub token
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`),                 // GitHub fine-grained token
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),                      // Anthropic key
	regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{32,}`),                  // OpenAI key
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                   // Slack token
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),                        // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`), // JSON web token
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|token)\s*[:=]\s*\S{8,}`),
}

// Sensitive reports whether text holds a credential or one of the words.
// Words match without regard to case.
func Sensitive(text string, words []string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	if len(words) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(lower, w) {
			return true
		}
	}
	return false
}
