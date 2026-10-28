package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
)

// fitMargin is the share of VRAM above which a model may not load: the
// runtime, the compute graph and other processes need the rest.
const fitMargin = 0.92

// modelGPUs returns the detected GPUs that a model runs on with layout,
// following the generator's placement (llamaswap.place).
func modelGPUs(layout string, embedding bool, hw hardware.Config, gpus []gpu.GPU) []gpu.GPU {
	sel := slices.Clone(hw.GPUs)
	slices.Sort(sel)
	pos := -1 // -1 means every selected GPU
	switch {
	case layout == string(hardware.LayoutSpan):
	case strings.HasPrefix(layout, "gpu:"):
		pos, _ = strconv.Atoi(strings.TrimPrefix(layout, "gpu:"))
	case embedding:
		pos = len(sel) - 1
	case hw.Layout == hardware.LayoutPin:
		pos = 0
	}
	var out []gpu.GPU
	for i, idx := range sel {
		if (pos < 0 || i == pos) && idx >= 0 && idx < len(gpus) {
			out = append(out, gpus[idx])
		}
	}
	return out
}

// contextNote describes the context size and its memory cost for the
// values in the form. It is the description of the context size field.
func contextNote(m client.Model, f *settingsForm, hw hardware.Config, gpus []gpu.GPU) string {
	if m.Meta == nil || m.Meta.ContextLength == 0 {
		return "Tokens. The model file gives no native context, so the memory cost is unknown."
	}
	native := m.Meta.ContextLength
	ctx := native
	var lines []string
	if n, err := strconv.Atoi(strings.TrimSpace(f.CtxSize)); err == nil && n > 0 {
		ctx = n
		lines = append(lines, fmt.Sprintf("Native context: %s tokens. Empty uses all of it.", thousands(native)))
	} else {
		lines = append(lines, fmt.Sprintf("Empty uses the native context: %s tokens.", thousands(native)))
	}
	if ctx > native {
		lines = append(lines, "This is over the native context, so answers can get worse.")
	}
	cost := m.Meta.ContextBytes(ctx, f.KVTypeK, f.KVTypeV)
	used := modelGPUs(f.Layout, f.Embedding, hw, gpus)
	var vram int64
	for _, g := range used {
		if g.VRAMTotal == 0 {
			vram = 0
			break
		}
		vram += int64(g.VRAMTotal)
	}
	total := cost + m.Size
	line := fmt.Sprintf("At %s tokens: context about %s + weights %s = %s", thousands(ctx), gib(cost), gib(m.Size), gib(total))
	if vram > 0 {
		line += fmt.Sprintf(" of %s VRAM on %d GPU(s).", gib(vram), len(used))
	} else {
		line += "."
	}
	lines = append(lines, line)
	if vram > 0 && float64(total) > fitMargin*float64(vram) {
		lines = append(lines, "This may not fit. Use a smaller context, a q8_0 or q4_0 KV cache, or span more GPUs.")
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

// thousands formats n with comma separators.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
