package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/models"
)

// settingsForm holds model settings as the strings and lists that form
// fields edit. An empty string means "not set".
type settingsForm struct {
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
	Fit             string // quality (or empty) or context
	Preset          string // empty, or one of models.Presets
	Args            string // space separated
}

func fmtFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func fmtInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func formFromSettings(s models.Settings) settingsForm {
	f := settingsForm{
		Layout:          s.Layout,
		Embedding:       s.Embedding,
		Hidden:          s.Hidden,
		Temperature:     fmtFloat(s.Sampling.Temperature),
		TopK:            fmtInt(s.Sampling.TopK),
		TopP:            fmtFloat(s.Sampling.TopP),
		MinP:            fmtFloat(s.Sampling.MinP),
		RepeatPenalty:   fmtFloat(s.Sampling.RepeatPenalty),
		PresencePenalty: fmtFloat(s.Sampling.PresencePenalty),
		SpecTypes:       append([]string(nil), s.Speculative.Types...),
		DraftModel:      s.Speculative.DraftModel,
		DraftMax:        fmtInt(s.Speculative.DraftMax),
		DraftMin:        fmtInt(s.Speculative.DraftMin),
		NgramModMin:     fmtInt(s.Speculative.NgramModMin),
		NgramModMax:     fmtInt(s.Speculative.NgramModMax),
		KVTypeK:         s.KVCache.TypeK,
		KVTypeV:         s.KVCache.TypeV,
		Fit:             string(s.Fit),
		Preset:          s.Preset,
		Args:            strings.Join(s.Args, " "),
	}
	if s.CtxSize > 0 {
		f.CtxSize = strconv.Itoa(s.CtxSize)
	}
	if s.Parallel > 0 {
		f.Parallel = strconv.Itoa(s.Parallel)
	}
	split := make([]string, len(s.TensorSplit))
	for i, v := range s.TensorSplit {
		split[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	f.TensorSplit = strings.Join(split, ",")
	return f
}

func parseFloat(name, v string) (*float64, error) {
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

func parseInt(name, v string) (*int, error) {
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

// toSettings converts the form back. The API validates the ranges.
func (f settingsForm) toSettings() (models.Settings, error) {
	s := models.Settings{
		Layout:    f.Layout,
		Embedding: f.Embedding,
		Hidden:    f.Hidden,
		Args:      strings.Fields(f.Args),
		KVCache:   models.KVCache{TypeK: f.KVTypeK, TypeV: f.KVTypeV},
		Fit:       models.FitMode(f.Fit),
		Preset:    f.Preset,
	}
	var err error
	if ctx, e := parseInt("context size", f.CtxSize); e != nil {
		return s, e
	} else if ctx != nil {
		s.CtxSize = *ctx
	}
	if n, e := parseInt("parallel requests", f.Parallel); e != nil {
		return s, e
	} else if n != nil {
		s.Parallel = *n
	}
	for _, part := range strings.FieldsFunc(f.TensorSplit, func(r rune) bool { return r == ',' || r == ' ' }) {
		v, e := parseFloat("tensor split", part)
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
		if *x.out, err = parseFloat(x.name, x.in); err != nil {
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
		if *x.out, err = parseInt(x.name, x.in); err != nil {
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

// numberCheck is a form validator for optional numbers.
func numberCheck(whole bool) func(string) error {
	return func(v string) error {
		var err error
		if whole {
			_, err = parseInt("value", v)
		} else {
			_, err = parseFloat("value", v)
		}
		return err
	}
}
