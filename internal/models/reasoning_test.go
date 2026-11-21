package models

import (
	"os"
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
		{"qwen3.8 levels through an alias, and a switch for off", `{%- if enable_thinking is undefined or enable_thinking is true %}
{%- set resolved_reasoning_effort = reasoning_effort|default('xhigh') %}
{%- if resolved_reasoning_effort not in ('xhigh', 'medium', 'low') %}
{{- raise_exception('Unexpected reasoning effort ' ~ reasoning_effort ~ '. Supported types are xhigh (default), medium, and low.') }}
{%- if resolved_reasoning_effort == 'xhigh' %}
{%- elif resolved_reasoning_effort == 'low' %}`,
			&ReasoningControl{Var: "reasoning_effort", Values: []string{"xhigh", "medium", "low"}, OffVar: "enable_thinking"}},
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

// The template of thinkingmachines/Inkling-2-Mini keeps its levels as the
// keys of a map, and reaches reasoning_effort through two aliases.
func TestDetectReasoningLevelsFromAMap(t *testing.T) {
	b, err := os.ReadFile("testdata/inkling2-mini.jinja")
	if err != nil {
		t.Fatal(err)
	}
	want := &ReasoningControl{Var: "reasoning_effort", Values: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}}
	if got := DetectReasoning(string(b)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// A map that the variable is not looked up in gives no levels.
	other := `{%- set roles = {"user": "U", "tool": "T"} -%}{%- if message.role not in roles -%}{%- endif -%}{{ reasoning_effort }}`
	if got := DetectReasoning(other); got == nil || len(got.Values) != 0 {
		t.Errorf("got %+v, want no levels", got)
	}
}

func TestReasoningCheckAndFlags(t *testing.T) {
	levels := &ReasoningControl{Var: "reasoning_effort", Values: []string{"low", "high", "xhigh"}}
	if err := (Reasoning{Var: "reasoning_effort", Value: "xhigh"}).CheckAgainst(levels); err != nil {
		t.Error(err)
	}
	withOff := &ReasoningControl{Var: "reasoning_effort", Values: []string{"low"}, OffVar: "enable_thinking"}
	if err := (Reasoning{Var: "enable_thinking", Value: "off"}).CheckAgainst(withOff); err != nil {
		t.Errorf("off through the switch: %v", err)
	}
	if err := (Reasoning{Var: "enable_thinking", Value: "on"}).CheckAgainst(withOff); err == nil {
		t.Error("on through the switch passed; the levels turn it on")
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
