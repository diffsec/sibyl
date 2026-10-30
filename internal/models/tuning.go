package models

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Sampling holds the server-side default sampling values for a model. A
// client can still override them per request through the OpenAI API. A nil
// field leaves the llama-server default in place.
type Sampling struct {
	Temperature     *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	TopK            *int     `yaml:"top_k,omitempty" json:"top_k,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty" json:"top_p,omitempty"`
	MinP            *float64 `yaml:"min_p,omitempty" json:"min_p,omitempty"`
	RepeatPenalty   *float64 `yaml:"repeat_penalty,omitempty" json:"repeat_penalty,omitempty"`
	PresencePenalty *float64 `yaml:"presence_penalty,omitempty" json:"presence_penalty,omitempty"`
}

// Speculative configures speculative decoding.
type Speculative struct {
	// Types are llama.cpp --spec-type names, tried in order.
	Types []string `yaml:"types,omitempty" json:"types,omitempty"`
	// DraftModel is the ID of an installed model, for the draft-* types
	// that need a separate drafter (all except draft-mtp).
	DraftModel  string `yaml:"draft_model,omitempty" json:"draft_model,omitempty"`
	DraftMax    *int   `yaml:"draft_max,omitempty" json:"draft_max,omitempty"`
	DraftMin    *int   `yaml:"draft_min,omitempty" json:"draft_min,omitempty"`
	NgramModMin *int   `yaml:"ngram_mod_min,omitempty" json:"ngram_mod_min,omitempty"`
	NgramModMax *int   `yaml:"ngram_mod_max,omitempty" json:"ngram_mod_max,omitempty"`
}

// KVCache sets the KV cache element types. Quantized types need flash
// attention, which Sibyl always turns on.
type KVCache struct {
	TypeK string `yaml:"type_k,omitempty" json:"type_k,omitempty"`
	TypeV string `yaml:"type_v,omitempty" json:"type_v,omitempty"`
}

// MoE keeps the expert weights of a Mixture of Experts model in system RAM,
// and streams the experts that the router picks to the GPUs.
type MoE struct {
	// CPULayers keeps the experts of the first N layers in system RAM
	// (--n-cpu-moe). AllLayers keeps every layer's experts there
	// (--cpu-moe). They stay memory-mapped from the model file, so when
	// RAM is short the kernel drops the experts that go unused and reads
	// them back from disk when the router picks them.
	CPULayers int `yaml:"cpu_layers,omitempty" json:"cpu_layers,omitempty"`
	// CacheMiB is a GPU cache for the experts in system RAM
	// (--moe-cache-mib). llama.cpp copies each expert that a token uses
	// into it, and keeps the most used ones. 0 turns it off.
	CacheMiB int `yaml:"cache_mib,omitempty" json:"cache_mib,omitempty"`
}

// AllLayers is the MoE.CPULayers value that keeps every layer's experts in
// system RAM.
const AllLayers = -1

// Limits of the MoE settings.
const (
	maxMoELayers   = 4096
	maxMoECacheMiB = 1 << 20
)

// SpecTypes are the --spec-type names in llama.cpp b11889
// (common/speculative.cpp, common_speculative_type_from_name_map).
var SpecTypes = []string{
	"none", "draft-simple", "draft-eagle3", "draft-mtp", "draft-dflash", "draft-dspark",
	"ngram-simple", "ngram-map-k", "ngram-map-k4v", "ngram-mod", "ngram-cache",
}

// specNeedsDraftModel lists the types that load a separate drafter model.
var specNeedsDraftModel = []string{"draft-simple", "draft-eagle3", "draft-dflash", "draft-dspark"}

// CacheTypes are the KV cache types that llama-server accepts.
var CacheTypes = []string{"f32", "f16", "bf16", "q8_0", "q4_0", "q4_1", "iq4_nl", "q5_0", "q5_1"}

// typedFlags are the llama-server flags, with every alias, that the typed
// settings own. Raw args must not set them, so each value has one source.
var typedFlags = []string{
	"--temp", "--temperature", "--top-k", "--top-p", "--min-p", "--repeat-penalty", "--presence-penalty",
	"--spec-type", "--spec-draft-model", "-md", "--model-draft",
	"--spec-draft-n-max", "--draft", "--draft-n", "--draft-max",
	"--spec-draft-n-min", "--draft-min", "--draft-n-min",
	"--spec-ngram-mod-n-min", "--spec-ngram-mod-n-max",
	"-ctk", "--cache-type-k", "-ctv", "--cache-type-v",
	"-c", "--ctx-size", "--embedding", "--embeddings",
	"-np", "--parallel", "-ts", "--tensor-split",
	"-cmoe", "--cpu-moe", "-ncmoe", "--n-cpu-moe", "--moe-cache-mib",
}

func checkFloat(name string, v *float64, lo, hi float64) error {
	if v != nil && (*v < lo || *v > hi) {
		return fmt.Errorf("%s: %v is outside %v to %v", name, *v, lo, hi)
	}
	return nil
}

func checkInt(name string, v *int, lo, hi int) error {
	if v != nil && (*v < lo || *v > hi) {
		return fmt.Errorf("%s: %d is outside %d to %d", name, *v, lo, hi)
	}
	return nil
}

// Validate checks every sampling value against a sane range.
func (s Sampling) Validate() error {
	for _, err := range []error{
		checkFloat("sampling.temperature", s.Temperature, 0, 5),
		checkInt("sampling.top_k", s.TopK, 0, 1000),
		checkFloat("sampling.top_p", s.TopP, 0, 1),
		checkFloat("sampling.min_p", s.MinP, 0, 1),
		checkFloat("sampling.repeat_penalty", s.RepeatPenalty, 0, 5),
		checkFloat("sampling.presence_penalty", s.PresencePenalty, -2, 2),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the speculative settings. It does not check that the draft
// model is installed; the generator does that against the model list.
func (s Speculative) Validate() error {
	needsDraft := false
	for _, t := range s.Types {
		if !slices.Contains(SpecTypes, t) {
			return fmt.Errorf("speculative.types: %q is not one of %v", t, SpecTypes)
		}
		if slices.Contains(specNeedsDraftModel, t) {
			needsDraft = true
		}
	}
	if needsDraft && s.DraftModel == "" {
		return fmt.Errorf("speculative.draft_model: required for %v", specNeedsDraftModel)
	}
	if !needsDraft && s.DraftModel != "" {
		return fmt.Errorf("speculative.draft_model: set, but no type in %v uses it", s.Types)
	}
	for _, err := range []error{
		checkInt("speculative.draft_max", s.DraftMax, 1, 64),
		checkInt("speculative.draft_min", s.DraftMin, 0, 64),
		checkInt("speculative.ngram_mod_min", s.NgramModMin, 1, 64),
		checkInt("speculative.ngram_mod_max", s.NgramModMax, 1, 64),
	} {
		if err != nil {
			return err
		}
	}
	if s.DraftMax != nil && s.DraftMin != nil && *s.DraftMin > *s.DraftMax {
		return fmt.Errorf("speculative.draft_min %d is larger than draft_max %d", *s.DraftMin, *s.DraftMax)
	}
	return nil
}

// Validate checks the cache types.
func (k KVCache) Validate() error {
	for name, v := range map[string]string{"kv_cache.type_k": k.TypeK, "kv_cache.type_v": k.TypeV} {
		if v != "" && !slices.Contains(CacheTypes, v) {
			return fmt.Errorf("%s: %q is not one of %v", name, v, CacheTypes)
		}
	}
	return nil
}

// Validate checks the MoE settings. It does not check that the model has
// experts; the System API does that against the model file.
func (m MoE) Validate() error {
	if m.CPULayers < AllLayers || m.CPULayers > maxMoELayers {
		return fmt.Errorf("moe.cpu_layers: %d is outside %d (all) to %d", m.CPULayers, AllLayers, maxMoELayers)
	}
	if err := checkInt("moe.cache_mib", &m.CacheMiB, 0, maxMoECacheMiB); err != nil {
		return err
	}
	if m.CacheMiB > 0 && m.CPULayers == 0 {
		return errors.New("moe.cache_mib: needs moe.cpu_layers, because the cache holds experts from system RAM")
	}
	return nil
}

// Enabled reports whether any MoE setting is set.
func (m MoE) Enabled() bool { return m != MoE{} }

// Flags renders the MoE settings.
func (m MoE) Flags() []string {
	var out []string
	switch {
	case m.CPULayers == AllLayers:
		out = append(out, "--cpu-moe")
	case m.CPULayers > 0:
		out = append(out, "--n-cpu-moe", strconv.Itoa(m.CPULayers))
	}
	if m.CacheMiB > 0 {
		out = append(out, "--moe-cache-mib", strconv.Itoa(m.CacheMiB))
	}
	return out
}

// Flags renders the sampling settings as llama-server arguments.
func (s Sampling) Flags() []string {
	var out []string
	addF := func(flag string, v *float64) {
		if v != nil {
			out = append(out, flag, strconv.FormatFloat(*v, 'f', -1, 64))
		}
	}
	addI := func(flag string, v *int) {
		if v != nil {
			out = append(out, flag, strconv.Itoa(*v))
		}
	}
	addF("--temp", s.Temperature)
	addI("--top-k", s.TopK)
	addF("--top-p", s.TopP)
	addF("--min-p", s.MinP)
	addF("--repeat-penalty", s.RepeatPenalty)
	addF("--presence-penalty", s.PresencePenalty)
	return out
}

// Flags renders the speculative settings. draftPath is the drafter's path
// inside the inference container, or empty when no type needs one.
func (s Speculative) Flags(draftPath string) []string {
	if len(s.Types) == 0 {
		return nil
	}
	out := []string{"--spec-type", strings.Join(s.Types, ",")}
	if draftPath != "" {
		out = append(out, "--spec-draft-model", draftPath)
	}
	for _, f := range []struct {
		flag string
		v    *int
	}{
		{"--spec-draft-n-max", s.DraftMax},
		{"--spec-draft-n-min", s.DraftMin},
		{"--spec-ngram-mod-n-min", s.NgramModMin},
		{"--spec-ngram-mod-n-max", s.NgramModMax},
	} {
		if f.v != nil {
			out = append(out, f.flag, strconv.Itoa(*f.v))
		}
	}
	return out
}

// Flags renders the KV cache settings.
func (k KVCache) Flags() []string {
	var out []string
	if k.TypeK != "" {
		out = append(out, "--cache-type-k", k.TypeK)
	}
	if k.TypeV != "" {
		out = append(out, "--cache-type-v", k.TypeV)
	}
	return out
}
