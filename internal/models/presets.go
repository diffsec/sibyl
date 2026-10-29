package models

import (
	"fmt"
	"slices"
	"strings"
)

// Presets are the sampling presets for a use case. Each starts from the
// model's recommended sampling and adjusts it.
var Presets = []string{"general", "coding", "agentic", "writing"}

func f64(v float64) *float64 { return &v }
func intp(v int) *int        { return &v }

// llamaDefaults are llama-server's own defaults, the base for a model with
// no recommendation.
var llamaDefaults = Sampling{Temperature: f64(0.8), TopK: intp(40), TopP: f64(0.95), MinP: f64(0.05)}

// family is the recommended sampling of a model family, for a model file
// that carries none (general.sampling.*).
type family struct {
	prefixes []string // of the architecture or the model name, lowercase
	base     Sampling
	// fixed means the vendor gives one setting for every use, so the
	// presets do not change it.
	fixed  bool
	source string
}

var families = []family{
	{[]string{"gpt-oss"}, Sampling{Temperature: f64(1), TopP: f64(1)}, true,
		"OpenAI gpt-oss model card: temperature 1.0, top_p 1.0"},
	{[]string{"qwen"}, Sampling{Temperature: f64(0.6), TopK: intp(20), TopP: f64(0.95), MinP: f64(0)}, false,
		"Qwen3 model card, thinking mode"},
	{[]string{"gemma"}, Sampling{Temperature: f64(1), TopK: intp(64), TopP: f64(0.95), MinP: f64(0)}, false,
		"Gemma 3 recommended settings"},
	{[]string{"deepseek"}, Sampling{Temperature: f64(0.6), TopP: f64(0.95)}, false,
		"DeepSeek-R1 usage recommendations"},
	{[]string{"mistral", "devstral", "magistral"}, Sampling{Temperature: f64(0.15)}, false,
		"Mistral Small 3 model card"},
	{[]string{"llama"}, Sampling{Temperature: f64(0.6), TopP: f64(0.9)}, false,
		"Meta Llama 3 generation_config.json"},
}

// familyOf finds the family of a model by its name, then its architecture.
func familyOf(m *Meta) (family, bool) {
	if m == nil {
		return family{}, false
	}
	for _, key := range []string{strings.ToLower(m.Name), strings.ToLower(m.Architecture)} {
		for _, f := range families {
			for _, p := range f.prefixes {
				if key != "" && strings.HasPrefix(key, p) {
					return f, true
				}
			}
		}
	}
	return family{}, false
}

// Merge fills the fields of s that are nil from base.
func (s Sampling) Merge(base Sampling) Sampling {
	if s.Temperature == nil {
		s.Temperature = base.Temperature
	}
	if s.TopK == nil {
		s.TopK = base.TopK
	}
	if s.TopP == nil {
		s.TopP = base.TopP
	}
	if s.MinP == nil {
		s.MinP = base.MinP
	}
	if s.RepeatPenalty == nil {
		s.RepeatPenalty = base.RepeatPenalty
	}
	if s.PresencePenalty == nil {
		s.PresencePenalty = base.PresencePenalty
	}
	return s
}

// Recommended returns the sampling that a model's file recommends, else
// its family's, else llama-server's defaults, and where it came from.
func Recommended(m *Meta) (Sampling, string) {
	base, source := llamaDefaults, "llama.cpp defaults"
	f, known := familyOf(m)
	if known {
		base, source = f.base.Merge(llamaDefaults), f.source
	}
	if m != nil && !m.Sampling.IsZero() {
		return m.Sampling.Merge(base), "the model file"
	}
	return base, source
}

// IsZero reports whether no field is set.
func (s Sampling) IsZero() bool { return s == Sampling{} }

// PresetSampling returns a preset's sampling for a model. The presets
// adjust the model's recommendation:
//
//   - general: the recommendation as it is.
//   - coding and agentic: temperature at most 0.6, so code and tool calls
//     are more consistent.
//   - writing: temperature at least 0.8, and min_p at least 0.05, which
//     keeps a higher temperature coherent.
//
// A family whose vendor gives one setting for every use, such as gpt-oss,
// keeps it for every preset.
func PresetSampling(preset string, m *Meta) (Sampling, error) {
	if !slices.Contains(Presets, preset) {
		return Sampling{}, fmt.Errorf("preset %q: use one of %s", preset, strings.Join(Presets, ", "))
	}
	s, _ := Recommended(m)
	if f, ok := familyOf(m); ok && f.fixed {
		return s, nil
	}
	temp := *s.Temperature
	switch preset {
	case "coding", "agentic":
		s.Temperature = f64(min(temp, 0.6))
	case "writing":
		s.Temperature = f64(max(temp, 0.8))
		if s.MinP == nil || *s.MinP < 0.05 {
			s.MinP = f64(0.05)
		}
	}
	return s, nil
}

// EffectiveSampling returns the sampling that a model runs with: the owner's
// values first, then the preset's, if one is set.
func (s Settings) EffectiveSampling(m *Meta) Sampling {
	if s.Preset == "" {
		return s.Sampling
	}
	p, err := PresetSampling(s.Preset, m)
	if err != nil {
		return s.Sampling
	}
	return s.Sampling.Merge(p)
}
