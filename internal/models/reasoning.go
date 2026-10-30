package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
	// OffVar is a switch variable that the same template reads next to the
	// levels, such as enable_thinking next to reasoning_effort. It gives
	// the choice off.
	OffVar string `json:"off_var,omitempty"`
}

// ReasoningChoice is one value that a form offers.
type ReasoningChoice struct {
	Var, Value string
}

// Choices are the values that a form offers, in the template's order.
func (c ReasoningControl) Choices() []ReasoningChoice {
	var out []ReasoningChoice
	if c.Switch {
		return []ReasoningChoice{{c.Var, ReasoningOn}, {c.Var, ReasoningOff}}
	}
	for _, v := range c.Values {
		out = append(out, ReasoningChoice{c.Var, v})
	}
	if c.OffVar != "" {
		out = append(out, ReasoningChoice{c.OffVar, ReasoningOff})
	}
	return out
}

// The values of a switch variable.
const (
	ReasoningOn  = "on"
	ReasoningOff = "off"
)

type reasoningVar struct {
	name   string
	sw     bool
	usedRE *regexp.Regexp
}

// reasoningVars are the template variables that Sibyl knows, in the order
// it looks for them.
var reasoningVars = []reasoningVar{
	{"reasoning_effort", false, regexp.MustCompile(`\breasoning_effort\b`)},
	{"enable_thinking", true, regexp.MustCompile(`\benable_thinking\b`)},
	// "thinking" is also a word in prompts and a message field, so only
	// its use as a template variable counts.
	{"thinking", true, regexp.MustCompile(`(?:\{%-?\s*(?:el)?if\s+(?:not\s+)?thinking\b|[^.\w'"\[]thinking\s+is\s+(?:defined|true|false|not\b))`)},
}

func findVar(name string) (reasoningVar, bool) {
	for _, v := range reasoningVars {
		if v.name == name {
			return v, true
		}
	}
	return reasoningVar{}, false
}

const literal = `['"]([A-Za-z0-9_-]{1,32})['"]`

var (
	literalRE = regexp.MustCompile(literal)
	valueRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// levels finds the values that a template compares name with: in
// name == "x", "x" == name, name in ("x", "y") and name | default("x").
// It follows {% set alias = name ... %}, because templates often resolve
// the default into another variable first.
func levels(template, name string) []string {
	names := []string{name}
	aliasRE := regexp.MustCompile(`set\s+(\w+)\s*=\s*` + regexp.QuoteMeta(name) + `\b`)
	for _, m := range aliasRE.FindAllStringSubmatch(template, -1) {
		if !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, n := range names {
		v := regexp.QuoteMeta(n)
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`\b` + v + `\s+(?:not\s+)?in\s+[\[(]([^\])]*)[\])]`),
		} {
			for _, m := range re.FindAllStringSubmatch(template, -1) {
				for _, lit := range literalRE.FindAllStringSubmatch(m[1], -1) {
					add(lit[1])
				}
			}
		}
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`\b` + v + `\s*(?:==|!=)\s*` + literal),
			regexp.MustCompile(literal + `\s*(?:==|!=)\s*` + v + `\b`),
			regexp.MustCompile(`\b` + v + `\s*\|\s*default\(\s*` + literal),
		} {
			for _, m := range re.FindAllStringSubmatch(template, -1) {
				add(m[1])
			}
		}
	}
	return out
}

// DetectReasoning reads a chat template and returns its reasoning control,
// or nil when it has none that Sibyl knows.
func DetectReasoning(template string) *ReasoningControl {
	var c *ReasoningControl
	for _, v := range reasoningVars {
		if !v.usedRE.MatchString(template) {
			continue
		}
		switch {
		case c == nil && !v.sw:
			c = &ReasoningControl{Var: v.name, Values: levels(template, v.name)}
		case c == nil:
			return &ReasoningControl{Var: v.name, Switch: true}
		case c.OffVar == "" && v.sw:
			c.OffVar = v.name
		}
	}
	return c
}

// Reasoning is a model's default reasoning: the template variable and its
// value. A request's own reasoning_effort or chat_template_kwargs wins.
type Reasoning struct {
	Var   string `yaml:"var" json:"var"`
	Value string `yaml:"value" json:"value"`
}

// Validate checks the variable and the form of the value.
func (r Reasoning) Validate() error {
	v, ok := findVar(r.Var)
	switch {
	case !ok:
		return fmt.Errorf("reasoning.var %q: Sibyl knows reasoning_effort, enable_thinking and thinking", r.Var)
	case v.sw && r.Value != ReasoningOn && r.Value != ReasoningOff:
		return fmt.Errorf("reasoning.value %q: %s is on or off", r.Value, r.Var)
	case !valueRE.MatchString(r.Value):
		return errors.New("reasoning.value: use 1 to 32 letters, digits, _ and -")
	}
	return nil
}

// CheckAgainst checks the setting against the model's template.
func (r Reasoning) CheckAgainst(c *ReasoningControl) error {
	if c == nil {
		return errors.New("reasoning: the model's chat template has no reasoning control that Sibyl knows")
	}
	choices := c.Choices()
	if slices.Contains(choices, ReasoningChoice{r.Var, r.Value}) {
		return nil
	}
	// A level variable whose values Sibyl could not list takes any level.
	if r.Var == c.Var && !c.Switch && len(c.Values) == 0 {
		return nil
	}
	var list []string
	for _, ch := range choices {
		list = append(list, ch.Var+"="+ch.Value)
	}
	if len(list) == 0 {
		list = append(list, c.Var+"=<any level>")
	}
	return fmt.Errorf("reasoning %s=%s: the model's chat template takes %s", r.Var, r.Value, strings.Join(list, ", "))
}

// Flags returns the llama-server flags of the setting.
func (r *Reasoning) Flags() []string {
	if r == nil {
		return nil
	}
	var v any = r.Value
	if rv, ok := findVar(r.Var); ok && rv.sw {
		v = r.Value == ReasoningOn
	}
	b, err := json.Marshal(map[string]any{r.Var: v})
	if err != nil {
		return nil
	}
	// llama-swap splits the command with POSIX shell rules, so the JSON is
	// quoted. Validate keeps quotes out of the value.
	return []string{"--chat-template-kwargs", "'" + string(b) + "'"}
}
