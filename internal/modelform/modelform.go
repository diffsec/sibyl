// Package modelform turns model settings into the strings and lists that a
// form edits, and back. The TUI and the web console share it, so both
// check a value the same way.
package modelform

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/models"
)

// Form holds model settings as the strings and lists that form fields
// edit. An empty string means "not set".
type Form struct {
	Layout          string
	CtxSize         string
	Parallel        string
	TensorSplit     string // comma separated
	Embedding       bool
	Hidden          bool
	Temperature     string
	TopK            string
	TopP            string
	MinP            string
	RepeatPenalty   string
	PresencePenalty string
	SpecTypes       []string
	DraftModel      string
	DraftMax        string
	DraftMin        string
	NgramModMin     string
	NgramModMax     string
	KVTypeK         string
	KVTypeV         string
	MoELayers       string // empty, "all", or a number of layers
	MoECacheMiB     string
	Fit             string // quality (or empty) or context
	Preset          string // empty, or one of models.Presets
	// Reasoning is the template variable and its value as var=value, such
	// as reasoning_effort=low or enable_thinking=off. A value alone uses
	// ReasoningVar. Empty leaves the template's default.
	Reasoning    string
	ReasoningVar string
	Args         string // space separated
}

func FmtFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func FmtInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func FromSettings(s models.Settings) Form {
	f := Form{
		Layout:          s.Layout,
		Embedding:       s.Embedding,
		Hidden:          s.Hidden,
		Temperature:     FmtFloat(s.Sampling.Temperature),
		TopK:            FmtInt(s.Sampling.TopK),
		TopP:            FmtFloat(s.Sampling.TopP),
		MinP:            FmtFloat(s.Sampling.MinP),
		RepeatPenalty:   FmtFloat(s.Sampling.RepeatPenalty),
		PresencePenalty: FmtFloat(s.Sampling.PresencePenalty),
		SpecTypes:       append([]string(nil), s.Speculative.Types...),
		DraftModel:      s.Speculative.DraftModel,
		DraftMax:        FmtInt(s.Speculative.DraftMax),
		DraftMin:        FmtInt(s.Speculative.DraftMin),
		NgramModMin:     FmtInt(s.Speculative.NgramModMin),
		NgramModMax:     FmtInt(s.Speculative.NgramModMax),
		KVTypeK:         s.KVCache.TypeK,
		KVTypeV:         s.KVCache.TypeV,
		MoELayers:       FmtMoELayers(s.MoE.CPULayers),
		Fit:             string(s.Fit),
		Preset:          s.Preset,

		Args: strings.Join(s.Args, " "),
	}
	if s.CtxSize > 0 {
		f.CtxSize = strconv.Itoa(s.CtxSize)
	}
	if s.Reasoning != nil {
		f.Reasoning = s.Reasoning.Var + "=" + s.Reasoning.Value
	}
	if s.Parallel > 0 {
		f.Parallel = strconv.Itoa(s.Parallel)
	}
	if s.MoE.CacheMiB > 0 {
		f.MoECacheMiB = strconv.Itoa(s.MoE.CacheMiB)
	}
	split := make([]string, len(s.TensorSplit))
	for i, v := range s.TensorSplit {
		split[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	f.TensorSplit = strings.Join(split, ",")
	return f
}

func FmtMoELayers(n int) string {
	switch {
	case n == models.AllLayers:
		return "all"
	case n > 0:
		return strconv.Itoa(n)
	}
	return ""
}

// ParseMoELayers reads the experts field: empty, "all", or a number of
// layers.
func ParseMoELayers(v string) (int, error) {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "all") {
		return models.AllLayers, nil
	}
	n, err := ParseInt("experts in system RAM", v)
	if err != nil {
		return 0, fmt.Errorf("experts in system RAM: %q is not \"all\" or a whole number", v)
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

func ParseFloat(name, v string) (*float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a number", name, v)
	}
	return &f, nil
}

func ParseInt(name, v string) (*int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a whole number", name, v)
	}
	return &n, nil
}

// Settings converts the form back. The API validates the ranges.
func (f Form) Settings() (models.Settings, error) {
	s := models.Settings{
		Layout:    f.Layout,
		Embedding: f.Embedding,
		Hidden:    f.Hidden,
		Args:      strings.Fields(f.Args),
		KVCache:   models.KVCache{TypeK: f.KVTypeK, TypeV: f.KVTypeV},
		Fit:       models.FitMode(f.Fit),
		Preset:    f.Preset,
	}
	if v := strings.TrimSpace(f.Reasoning); v != "" {
		name, value, ok := strings.Cut(v, "=")
		if !ok {
			name, value = f.ReasoningVar, v
		}
		if name == "" {
			return s, errors.New("reasoning: give the template variable as var=value")
		}
		// The API checks the value against the model's template.
		s.Reasoning = &models.Reasoning{Var: name, Value: value}
	}
	var err error
	if ctx, e := ParseInt("context size", f.CtxSize); e != nil {
		return s, e
	} else if ctx != nil {
		s.CtxSize = *ctx
	}
	if n, e := ParseInt("parallel requests", f.Parallel); e != nil {
		return s, e
	} else if n != nil {
		s.Parallel = *n
	}
	if s.MoE.CPULayers, err = ParseMoELayers(f.MoELayers); err != nil {
		return s, err
	}
	if n, e := ParseInt("expert GPU cache", f.MoECacheMiB); e != nil {
		return s, e
	} else if n != nil {
		s.MoE.CacheMiB = *n
	}
	for _, part := range strings.FieldsFunc(f.TensorSplit, func(r rune) bool { return r == ',' || r == ' ' }) {
		v, e := ParseFloat("tensor split", part)
		if e != nil {
			return s, e
		}
		s.TensorSplit = append(s.TensorSplit, *v)
	}
	floats := []struct {
		name string
		in   string
		out  **float64
	}{
		{"temperature", f.Temperature, &s.Sampling.Temperature},
		{"top p", f.TopP, &s.Sampling.TopP},
		{"min p", f.MinP, &s.Sampling.MinP},
		{"repeat penalty", f.RepeatPenalty, &s.Sampling.RepeatPenalty},
		{"presence penalty", f.PresencePenalty, &s.Sampling.PresencePenalty},
	}
	for _, x := range floats {
		if *x.out, err = ParseFloat(x.name, x.in); err != nil {
			return s, err
		}
	}
	ints := []struct {
		name string
		in   string
		out  **int
	}{
		{"top k", f.TopK, &s.Sampling.TopK},
		{"draft max", f.DraftMax, &s.Speculative.DraftMax},
		{"draft min", f.DraftMin, &s.Speculative.DraftMin},
		{"ngram-mod min", f.NgramModMin, &s.Speculative.NgramModMin},
		{"ngram-mod max", f.NgramModMax, &s.Speculative.NgramModMax},
	}
	for _, x := range ints {
		if *x.out, err = ParseInt(x.name, x.in); err != nil {
			return s, err
		}
	}
	for _, t := range f.SpecTypes {
		if t != "none" {
			s.Speculative.Types = append(s.Speculative.Types, t)
		}
	}
	if len(s.Speculative.Types) > 0 {
		s.Speculative.DraftModel = f.DraftModel
	}
	return s, nil
}

// NumberCheck is a form validator for optional numbers.
func NumberCheck(whole bool) func(string) error {
	return func(v string) error {
		var err error
		if whole {
			_, err = ParseInt("value", v)
		} else {
			_, err = ParseFloat("value", v)
		}
		return err
	}
}
