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

// EmbeddingContext is the context that an embedding model gets when its
// context size is not set. Embedding inputs are short, and the native
// context of 32,768 tokens costs about 4 GiB on a 0.6B model.
const EmbeddingContext = 8192

// Fit is the context size and KV cache types that a model runs with.
type Fit struct {
	// Ctx is llama-server's --ctx-size: the context of all slots together.
	Ctx   int    `json:"ctx"`
	TypeK string `json:"type_k,omitempty"` // empty is f16, llama-server's default
	TypeV string `json:"type_v,omitempty"`
	// Slots is the number of requests that run at once, and SlotCtx the
	// context that each one is sure to get: Ctx divided by Slots. The
	// slots share one cache, so a request alone can use more.
	Slots   int `json:"slots,omitempty"`
	SlotCtx int `json:"slot_ctx,omitempty"`
	// Reserved is the VRAM that other models keep on the same GPUs.
	Reserved int64 `json:"reserved_bytes,omitempty"`
	// TensorSplit is the share of the layers on each GPU, when the model
	// spans GPUs.
	TensorSplit []float64 `json:"tensor_split,omitempty"`
	// Auto is true when Sibyl chose the context size or the cache types.
	Auto bool    `json:"auto"`
	Mode FitMode `json:"mode,omitempty"`
	// Fits is false when the weights and the context need more than the
	// GPUs hold, so llama.cpp keeps part of the model in system RAM.
	Fits bool   `json:"fits"`
	Note string `json:"note,omitempty"`
}

// FitInput is what FitContext measures against.
type FitInput struct {
	Mode FitMode
	// Weights are the bytes of weights on the GPUs, and VRAM the memory of
	// the GPUs. Reserved is the memory that other models keep on them.
	Weights, VRAM, Reserved int64
	// Ctx and the cache types are the owner's settings: a value set there
	// is kept. A Ctx of 0 means the largest context that fits, up to the
	// native context for each slot.
	Ctx          int
	TypeK, TypeV string
	// Slots is the number of parallel requests. 0 counts as 1.
	Slots int
}

// cacheSteps are the KV cache types that FitContext tries, in order.
var cacheSteps = []string{"", "q8_0", "q4_0"}

// FitContext chooses the context size and KV cache types for a model.
func (m Meta) FitContext(in FitInput) (f Fit) {
	mode := in.Mode
	if mode == "" {
		mode = FitQuality
	}
	slots := max(in.Slots, 1)
	native := m.ContextLength
	target := in.Ctx
	if target <= 0 {
		target = native * slots
	}
	f = Fit{Ctx: in.Ctx, TypeK: in.TypeK, TypeV: in.TypeV, Mode: mode, Slots: slots, Reserved: in.Reserved}
	defer func() {
		if f.Ctx > 0 {
			f.SlotCtx = f.Ctx / slots
		}
	}()
	if in.VRAM <= 0 || native <= 0 || target <= 0 {
		// Nothing to measure against: llama-server's defaults apply.
		f.Note = "the VRAM or the native context is not known"
		return f
	}
	budget := int64(FitMargin*float64(in.VRAM)) - in.Reserved - in.Weights
	steps := [][2]string{{in.TypeK, in.TypeV}}
	if in.TypeK == "" && in.TypeV == "" && mode == FitContext {
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
	f.Auto = f.TypeK != in.TypeK || f.TypeV != in.TypeV
	n := tokens(pick)
	f.Fits = n >= int64(target)

	switch {
	case in.Ctx > 0:
		// The owner's context stays, even when it does not fit.
		f.Ctx = in.Ctx
	case f.Fits:
		f.Ctx, f.Auto = target, true
	default:
		// Each slot gets a multiple of minContext.
		per := max(int(n)/slots/minContext*minContext, minContext)
		f.Ctx, f.Auto = min(per*slots, target), true
		f.Fits = int64(f.Ctx) <= n
	}
	switch {
	case budget <= 0:
		f.Note = "the weights alone need more than the GPUs hold"
	case !f.Fits:
		f.Note = fmt.Sprintf("%d tokens do not fit; llama.cpp keeps part of the model in system RAM", f.Ctx)
	case slots > 1 && f.Ctx/slots < native:
		f.Note = fmt.Sprintf("the native context of %d tokens does not fit for each of %d slots", native, slots)
	case f.Ctx < native:
		f.Note = fmt.Sprintf("the native context of %d tokens does not fit", native)
	}
	return f
}

// SplitFor returns the share of the layers for each GPU of a spanning
// model: in proportion to the memory that each GPU has left for it, in
// MiB. vram and reserved are per GPU, in the same order.
func SplitFor(vram, reserved []int64) []float64 {
	out := make([]float64, len(vram))
	for i := range vram {
		left := int64(FitMargin*float64(vram[i])) - reserved[i]
		out[i] = float64(max(left, 0) >> 20)
	}
	return out
}
