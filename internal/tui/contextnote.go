package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

// contextNote describes the context size and its memory cost for the
// values in the form. It is the description of the context size field.
func contextNote(m client.Model, f *settingsForm, hw hardware.Config, gpus []gpu.GPU) string {
	if m.Meta == nil || m.Meta.ContextLength == 0 {
		return "Tokens. The model file gives no native context, so the memory cost is unknown."
	}
	native := m.Meta.ContextLength
	used := hardware.ModelGPUs(f.Layout, f.Embedding, hw, gpus)
	vram := hardware.VRAM(used)
	set, _ := strconv.Atoi(strings.TrimSpace(f.CtxSize))
	weights := m.Meta.Weights(m.Size)
	fit := m.Meta.FitContext(models.FitMode(f.Fit), weights, vram, max(set, 0), f.KVTypeK, f.KVTypeV)
	ctx, typeK, typeV := fit.Ctx, fit.TypeK, fit.TypeV
	if ctx == 0 {
		ctx = native
	}
	var lines []string
	switch {
	case set > 0:
		lines = append(lines, fmt.Sprintf("Native context: %s tokens. Empty lets Sibyl choose.", thousands(native)))
	case fit.Auto && !f.Embedding:
		cache := "an f16 KV cache"
		if typeK != "" {
			cache = "a " + typeK + " KV cache"
		}
		lines = append(lines, fmt.Sprintf("Empty: Sibyl uses %s tokens with %s, the most that fits (native %s).",
			thousands(ctx), cache, thousands(native)))
	default:
		lines = append(lines, fmt.Sprintf("Empty uses the native context: %s tokens.", thousands(native)))
	}
	if ctx > native {
		lines = append(lines, "This is over the native context, so answers can get worse.")
	}
	cost := m.Meta.ContextBytes(ctx, typeK, typeV)
	total := cost + weights
	line := fmt.Sprintf("At %s tokens: context about %s + weights %s = %s", thousands(ctx), gib(cost), gib(weights), gib(total))
	if vram > 0 {
		line += fmt.Sprintf(" of %s VRAM on %d GPU(s).", gib(vram), len(used))
	} else {
		line += "."
	}
	lines = append(lines, line)
	if host := m.Size - weights; host > 1<<30 {
		lines = append(lines, fmt.Sprintf("The embedding tables, %s, stay in system RAM.", gib(host)))
	}
	if vram > 0 && float64(total) > models.FitMargin*float64(vram) {
		lines = append(lines, "This is more than the GPUs hold, so llama.cpp keeps part of it in system RAM, which is slower.\n"+
			"A smaller context, a q8_0 or q4_0 KV cache, or more GPUs keeps it all on the GPUs.")
	}
	return strings.Join(lines, "\n")
}

// samplingHint is the placeholder of a sampling field: the value that
// applies when the field is empty.
func samplingHint(file *float64, llamaDefault string) string {
	if file != nil {
		return "model file: " + strconv.FormatFloat(*file, 'f', -1, 64)
	}
	return "llama.cpp default: " + llamaDefault
}

// presetHint is the placeholder of a sampling field when a preset is set.
func presetHint(preset string, v *float64) string {
	if v == nil {
		return "llama.cpp default"
	}
	return preset + ": " + strconv.FormatFloat(*v, 'f', -1, 64)
}

// presetNote describes the preset choice for a model.
func presetNote(meta *models.Meta) string {
	_, source := models.Recommended(meta)
	return "Starts from " + source + ". coding and agentic cap the temperature at 0.6; writing raises it to at\n" +
		"least 0.8 with min_p 0.05. Clients pick a preset per request with <model>:<preset>."
}

// thousands formats n with comma separators.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
