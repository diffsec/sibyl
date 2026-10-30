package models

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectReasoning(t *testing.T) {
	for _, c := range []struct {
		name, template string
		want           *ReasoningControl
	}{
		{"gpt-oss prints the value", `{%- if reasoning_effort is defined %}{{- "Reasoning: " + reasoning_effort }}{%- else %}{{- "Reasoning: medium" }}{%- endif %}`,
			&ReasoningControl{Var: "reasoning_effort"}},
		{"levels by comparison", `{%- set e = reasoning_effort | default("medium") %}{% if reasoning_effort == 'xhigh' %}think hard{% elif reasoning_effort == "low" %}brief{% endif %}`,
			&ReasoningControl{Var: "reasoning_effort", Values: []string{"xhigh", "low", "medium"}}},
		{"levels in a list", `{% if reasoning_effort not in ["minimal", "low", "high"] %}{{ raise_exception("bad") }}{% endif %}`,
			&ReasoningControl{Var: "reasoning_effort", Values: []string{"minimal", "low", "high"}}},
		{"qwen3 switch", `{%- if enable_thinking is defined and enable_thinking is false %}<think>\n\n</think>{%- endif %}`,
			&ReasoningControl{Var: "enable_thinking", Switch: true}},
		{"deepseek switch", `{%- if thinking %}<think>{%- else %}</think>{%- endif %}`,
			&ReasoningControl{Var: "thinking", Switch: true}},
		{"thinking only as a word and a field", `{{ message.thinking }} The assistant is thinking about it. {{ message['thinking'] }}`, nil},
		{"none", `{% for m in messages %}{{ m.content }}{% endfor %}`, nil},
	} {
		got := DetectReasoning(c.template)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestReasoningCheckAndFlags(t *testing.T) {
	levels := &ReasoningControl{Var: "reasoning_effort", Values: []string{"low", "high", "xhigh"}}
	if err := (Reasoning{Var: "reasoning_effort", Value: "xhigh"}).CheckAgainst(levels); err != nil {
		t.Error(err)
	}
	for _, r := range []Reasoning{{"reasoning_effort", "medium"}, {"enable_thinking", "on"}} {
		if err := r.CheckAgainst(levels); err == nil {
			t.Errorf("%+v passed against %+v", r, levels)
		}
	}
	if err := (Reasoning{Var: "reasoning_effort", Value: "anything"}).CheckAgainst(&ReasoningControl{Var: "reasoning_effort"}); err != nil {
		t.Errorf("a template with no list refused a value: %v", err)
	}
	if err := (Reasoning{Var: "reasoning_effort", Value: "low"}).CheckAgainst(nil); err == nil {
		t.Error("a model with no control took a setting")
	}
	got := strings.Join((&Reasoning{Var: "thinking", Value: "on"}).Flags(), " ")
	if got != `--chat-template-kwargs '{"thinking":true}'` {
		t.Errorf("flags %s", got)
	}
}
