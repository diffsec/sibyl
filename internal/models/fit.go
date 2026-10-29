package models

import "fmt"

// FitMode decides what an automatic context size gives up when the GPUs
// are short of memory.
type FitMode string

// The fit modes.
const (
	// FitQuality keeps the KV cache at f16 and fits the largest context.
	FitQuality FitMode = "quality"
	// FitContext quantizes the KV cache as far as it must, to f16, then
	// q8_0, then q4_0, to reach the native context.
	FitContext FitMode = "context"
)

// FitMargin is the share of the model's VRAM that weights and context can
// use. The runtime, the compute graph and other processes need the rest.
const FitMargin = 0.92

// minContext is the smallest context that an automatic fit chooses.
const minContext = 4096

// Fit is the context size and KV cache types that a model runs with.
type Fit struct {
	Ctx   int    `json:"ctx"`
	TypeK string `json:"type_k,omitempty"` // empty is f16, llama-server's default
	TypeV string `json:"type_v,omitempty"`
	// Auto is true when Sibyl chose the context size or the cache types.
	Auto bool    `json:"auto"`
	Mode FitMode `json:"mode,omitempty"`
	// Fits is false when the weights and the context need more than the
	// GPUs hold, so llama.cpp keeps part of the model in system RAM.
	Fits bool   `json:"fits"`
	Note string `json:"note,omitempty"`
}

// cacheSteps are the KV cache types that FitContext tries, in order.
var cacheSteps = []string{"", "q8_0", "q4_0"}

// FitContext chooses the context size and KV cache types for a model of
// weights bytes on GPUs with vram bytes. ctx and the cache types are the
// owner's settings: a value set there is kept. A ctx of 0 means the largest
// context that fits, up to the native context.
func (m Meta) FitContext(mode FitMode, weights, vram int64, ctx int, typeK, typeV string) Fit {
	if mode == "" {
		mode = FitQuality
	}
	native := m.ContextLength
	target := ctx
	if target <= 0 {
		target = native
	}
	f := Fit{Ctx: ctx, TypeK: typeK, TypeV: typeV, Mode: mode}
	if vram <= 0 || native <= 0 || target <= 0 {
		// Nothing to measure against: llama-server's defaults apply.
		f.Note = "the VRAM or the native context is not known"
		return f
	}
	budget := int64(FitMargin*float64(vram)) - weights
	steps := [][2]string{{typeK, typeV}}
	if typeK == "" && typeV == "" && mode == FitContext {
		steps = nil
		for _, t := range cacheSteps {
			steps = append(steps, [2]string{t, t})
		}
	}
	tokens := func(s [2]string) int64 {
		if budget <= 0 {
			return 0
		}
		return budget / m.BytesPerToken(s[0], s[1])
	}

	// The first step that reaches the target, else the step that gives
	// the most tokens. On a small model the compute buffer outweighs the
	// cache, so a quantized cache can give fewer tokens than f16.
	pick, found := steps[0], false
	for _, s := range steps {
		if tokens(s) >= int64(target) {
			pick, found = s, true
			break
		}
	}
	if !found {
		for _, s := range steps[1:] {
			if tokens(s) > tokens(pick) {
				pick = s
			}
		}
	}
	f.TypeK, f.TypeV = pick[0], pick[1]
	f.Auto = f.TypeK != typeK || f.TypeV != typeV
	n := tokens(pick)
	f.Fits = n >= int64(target)

	switch {
	case ctx > 0:
		// The owner's context stays, even when it does not fit.
		f.Ctx = ctx
	case f.Fits:
		f.Ctx, f.Auto = target, true
	default:
		f.Ctx, f.Auto = int(max(n/minContext*minContext, minContext)), true
		f.Ctx = min(f.Ctx, target)
		f.Fits = int64(f.Ctx) <= n
	}
	switch {
	case budget <= 0:
		f.Note = "the weights alone need more than the GPUs hold"
	case !f.Fits:
		f.Note = fmt.Sprintf("%d tokens do not fit; llama.cpp keeps part of the model in system RAM", f.Ctx)
	case f.Ctx < native:
		f.Note = fmt.Sprintf("the native context of %d tokens does not fit", native)
	}
	return f
}
