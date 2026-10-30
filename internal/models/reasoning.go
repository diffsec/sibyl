package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// ReasoningControl is how a model's chat template sets its reasoning. The
// variable and its values differ by family, so Sibyl reads them from the
// template in the GGUF file.
type ReasoningControl struct {
	// Var is the template variable, such as reasoning_effort.
	Var string `json:"var"`
	// Switch is true for a variable that turns reasoning on or off, such
	// as enable_thinking. The values are then on and off.
	Switch bool `json:"switch,omitempty"`
	// Values are the levels that the template compares Var with, such as
	// low, medium and high. Empty for a level variable means that the
	// template takes a value that Sibyl could not list.
	Values []string `json:"values,omitempty"`
}

// Options are the values that a form offers, in the template's order.
func (c ReasoningControl) Options() []string {
	if c.Switch {
		return []string{ReasoningOn, ReasoningOff}
	}
	return c.Values
}

// The values of a switch variable.
const (
	ReasoningOn  = "on"
	ReasoningOff = "off"
)

// reasoningVars are the template variables that Sibyl knows, in the order
// it looks for them. A template can read more than one; the first wins.
var reasoningVars = []struct {
	name   string
	sw     bool
	usedRE *regexp.Regexp
}{
	{"reasoning_effort", false, regexp.MustCompile(`\breasoning_effort\b`)},
	{"enable_thinking", true, regexp.MustCompile(`\benable_thinking\b`)},
	// "thinking" is also a word in prompts and a message field, so only
	// its use as a template variable counts.
	{"thinking", true, regexp.MustCompile(`(?:\{%-?\s*(?:el)?if\s+(?:not\s+)?thinking\b|[^.\w'"\[]thinking\s+is\s+(?:defined|true|false|not\b))`)},
}

// levelRE finds the values that a template compares a variable with: in
// var == "x", "x" == var, var in ["x", "y"] and var | default("x").
func levelREs(name string) []*regexp.Regexp {
	v := regexp.QuoteMeta(name)
	return []*regexp.Regexp{
		regexp.MustCompile(`\b` + v + `\s*(?:==|!=)\s*['"]([A-Za-z0-9_-]{1,32})['"]`),
		regexp.MustCompile(`['"]([A-Za-z0-9_-]{1,32})['"]\s*(?:==|!=)\s*` + v + `\b`),
		regexp.MustCompile(`\b` + v + `\s*\|\s*default\(\s*['"]([A-Za-z0-9_-]{1,32})['"]`),
	}
}

var (
	listRE    = regexp.MustCompile(`\breasoning_effort\s+(?:not\s+)?in\s+[\[(]([^\])]*)[\])]`)
	literalRE = regexp.MustCompile(`['"]([A-Za-z0-9_-]{1,32})['"]`)
	valueRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// DetectReasoning reads a chat template and returns its reasoning control,
// or nil when it has none that Sibyl knows.
func DetectReasoning(template string) *ReasoningControl {
	for _, v := range reasoningVars {
		if !v.usedRE.MatchString(template) {
			continue
		}
		c := &ReasoningControl{Var: v.name, Switch: v.sw}
		if v.sw {
			return c
		}
		add := func(s string) {
			if !slices.Contains(c.Values, s) {
				c.Values = append(c.Values, s)
			}
		}
		for _, re := range levelREs(v.name) {
			for _, m := range re.FindAllStringSubmatch(template, -1) {
				add(m[1])
			}
		}
		for _, m := range listRE.FindAllStringSubmatch(template, -1) {
			for _, lit := range literalRE.FindAllStringSubmatch(m[1], -1) {
				add(lit[1])
			}
		}
		return c
	}
	return nil
}

// Reasoning is a model's default reasoning: the template variable and its
// value. A request's own reasoning_effort or chat_template_kwargs wins.
type Reasoning struct {
	Var   string `yaml:"var" json:"var"`
	Value string `yaml:"value" json:"value"`
}

// Validate checks the variable and the form of the value.
func (r Reasoning) Validate() error {
	i := slices.IndexFunc(reasoningVars, func(v struct {
		name   string
		sw     bool
		usedRE *regexp.Regexp
	}) bool {
		return v.name == r.Var
	})
	if i < 0 {
		return fmt.Errorf("reasoning.var %q: Sibyl knows reasoning_effort, enable_thinking and thinking", r.Var)
	}
	if reasoningVars[i].sw {
		if r.Value != ReasoningOn && r.Value != ReasoningOff {
			return fmt.Errorf("reasoning.value %q: %s is on or off", r.Value, r.Var)
		}
		return nil
	}
	if !valueRE.MatchString(r.Value) {
		return errors.New("reasoning.value: use 1 to 32 letters, digits, _ and -")
	}
	return nil
}

// CheckAgainst checks the setting against the model's template.
func (r Reasoning) CheckAgainst(c *ReasoningControl) error {
	switch {
	case c == nil:
		return errors.New("reasoning: the model's chat template has no reasoning control that Sibyl knows")
	case c.Var != r.Var:
		return fmt.Errorf("reasoning.var %q: the model's chat template reads %s", r.Var, c.Var)
	case len(c.Options()) > 0 && !slices.Contains(c.Options(), r.Value):
		return fmt.Errorf("reasoning.value %q: the model's chat template takes %v", r.Value, c.Options())
	}
	return nil
}

// Flags returns the llama-server flags of the setting.
func (r *Reasoning) Flags() []string {
	if r == nil {
		return nil
	}
	var v any = r.Value
	for _, rv := range reasoningVars {
		if rv.name == r.Var && rv.sw {
			v = r.Value == ReasoningOn
		}
	}
	b, err := json.Marshal(map[string]any{r.Var: v})
	if err != nil {
		return nil
	}
	// llama-swap splits the command with POSIX shell rules, so the JSON is
	// quoted. Validate keeps quotes out of the value.
	return []string{"--chat-template-kwargs", "'" + string(b) + "'"}
}
