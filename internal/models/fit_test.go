package models

import (
	"testing"

	"github.com/diffsec/sibyl/internal/gguf"
)

// meta costs 1 MiB per token at f16, and less when quantized: K and V of
// 256 Ki elements each, so 2 bytes x 512 Ki at f16 plus 8 KiB of compute.
func fitMeta() Meta {
	return Meta{ContextLength: 100_000, KElems: 256 << 10, VElems: 256 << 10, MaxLayerKV: 1 << 10, AttentionLayers: 1}
}

func TestFitContext(t *testing.T) {
	m := fitMeta()
	gib := int64(1 << 30)
	per := m.BytesPerToken("", "")
	for _, c := range []struct {
		name     string
		mode     FitMode
		weights  int64
		vram     int64
		ctx      int
		k, v     string
		wantCtx  int
		wantK    string
		wantFits bool
		wantAuto bool
	}{
		// 200 GiB of VRAM: the native context fits at f16.
		{"roomy", FitQuality, 10 * gib, 200 * gib, 0, "", "", 100_000, "", true, true},
		// Quality keeps f16 and shortens the context.
		{"quality short", FitQuality, 10 * gib, 60 * gib, 0, "", "", 0, "", true, true},
		// Context quantizes until the native context fits.
		{"context q8", FitContext, 10 * gib, 70 * gib, 0, "", "", 100_000, "q8_0", true, true},
		{"context q4", FitContext, 10 * gib, 45 * gib, 0, "", "", 100_000, "q4_0", true, true},
		// The owner's settings stay.
		{"owner ctx", FitQuality, 10 * gib, 20 * gib, 50_000, "", "", 50_000, "", false, false},
		{"owner types", FitContext, 10 * gib, 60 * gib, 0, "q8_0", "q8_0", 0, "q8_0", true, true},
		{"owner ctx, context mode", FitContext, 10 * gib, 70 * gib, 100_000, "", "", 100_000, "q8_0", true, true},
		// The weights alone do not fit.
		{"no room", FitQuality, 30 * gib, 20 * gib, 0, "", "", 4096, "", false, true},
	} {
		f := m.FitContext(FitInput{Mode: c.mode, Weights: c.weights, VRAM: c.vram, Ctx: c.ctx, TypeK: c.k, TypeV: c.v})
		want := c.wantCtx
		if want == 0 {
			// The largest multiple of 4096 that fits.
			budget := int64(FitMargin*float64(c.vram)) - c.weights
			want = int(budget / m.BytesPerToken(c.wantK, c.wantK) / 4096 * 4096)
		}
		if f.Ctx != want || f.TypeK != c.wantK || f.TypeV != c.wantK || f.Fits != c.wantFits || f.Auto != c.wantAuto {
			t.Errorf("%s: %+v, want ctx %d type %q fits %v auto %v", c.name, f, want, c.wantK, c.wantFits, c.wantAuto)
		}
		if f.Ctx%4096 != 0 && f.Ctx != c.ctx && f.Ctx != m.ContextLength {
			t.Errorf("%s: ctx %d is not a multiple of 4096", c.name, f.Ctx)
		}
	}
	_ = per
	// A small cache: the compute buffer dominates, so quantizing gives
	// fewer tokens, and context mode keeps f16.
	small := Meta{ContextLength: 1 << 20, KElems: 1024, VElems: 1024, MaxLayerKV: 2048, AttentionLayers: 1}
	if f := small.FitContext(FitInput{Mode: FitContext, Weights: 1 * gib, VRAM: 2 * gib}); f.TypeK != "" {
		t.Errorf("small model: %+v", f)
	}
	if f := m.FitContext(FitInput{Mode: FitQuality, Weights: 1}); f.Auto || f.Ctx != 0 {
		t.Errorf("unknown VRAM: %+v", f)
	}
}

func TestFitSlotsAndReserved(t *testing.T) {
	m := fitMeta() // 1 MiB per token at f16, native 100,000
	gib := int64(1 << 30)
	per := m.BytesPerToken("", "")

	// 200 GiB holds 100,000 tokens but not 2 x 100,000: two slots get the
	// largest multiple of 4096 tokens each that fits.
	f := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 200 * gib, Slots: 2})
	budget := int64(FitMargin*float64(200*gib)) - 10*gib
	wantPer := int(budget/per) / 2 / 4096 * 4096
	if f.Ctx != 2*wantPer || f.SlotCtx != wantPer || f.Slots != 2 || !f.Fits {
		t.Errorf("two slots: %+v, want %d per slot", f, wantPer)
	}
	if f := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 400 * gib, Slots: 2}); f.Ctx != 200_000 || f.SlotCtx != 100_000 {
		t.Errorf("two native slots: %+v", f)
	}

	// Reserved memory comes off the budget.
	free := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 60 * gib})
	held := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 60 * gib, Reserved: 5 * gib})
	if held.Ctx >= free.Ctx || held.Reserved != 5*gib {
		t.Errorf("reserved: %d tokens, %d without", held.Ctx, free.Ctx)
	}
}

func TestFitDraftWeights(t *testing.T) {
	m := fitMeta()
	gib := int64(1 << 30)
	free := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 60 * gib})
	draft := m.FitContext(FitInput{Weights: 10 * gib, VRAM: 60 * gib, DraftWeights: 5 * gib})
	if draft.Ctx >= free.Ctx || draft.Draft != 5*gib+DraftOverhead {
		t.Errorf("draft: %d tokens, %d without, draft %d", draft.Ctx, free.Ctx, draft.Draft)
	}
}

// TestRecurrentState checks the recurrent state of Qwen3.8-27B against
// llama-server's log: "R (f32): 180.00 MiB, S (f32): 4608.00 MiB" for 4
// sequences and 7 rollback copies, and that the fit counts it.
func TestRecurrentState(t *testing.T) {
	vals := map[string]gguf.Value{
		"qwen35.block_count": {Num: 65}, "qwen35.nextn_predict_layers": {Num: 1}, "qwen35.full_attention_interval": {Num: 4},
		"qwen35.attention.head_count": {Num: 24}, "qwen35.attention.head_count_kv": {Num: 4},
		"qwen35.attention.key_length": {Num: 256}, "qwen35.attention.value_length": {Num: 256},
		"qwen35.context_length": {Num: 262144}, "qwen35.embedding_length": {Num: 5120},
		"qwen35.ssm.conv_kernel": {Num: 4}, "qwen35.ssm.inner_size": {Num: 6144},
		"qwen35.ssm.state_size": {Num: 128}, "qwen35.ssm.group_count": {Num: 16},
	}
	m := metaFrom("qwen35", vals)
	if m.RecurrentLayers != 48 || m.AttentionLayers != 16 {
		t.Fatalf("layers: %d recurrent, %d attention", m.RecurrentLayers, m.AttentionLayers)
	}
	if got, want := m.RecurrentBytes(0, 7), int64(180+4608)<<20; got != want {
		t.Errorf("state with 7 rollback copies = %d MiB, want %d MiB", got>>20, want>>20)
	}
	if got := m.RecurrentBytes(1, 0); got != m.RecurrentBytes(0, 7)/32 {
		t.Errorf("one sequence, no rollback = %d", got)
	}
	gib := int64(1 << 30)
	plain := m.FitContext(FitInput{Weights: 26 * gib, VRAM: 48 * gib})
	spec := m.FitContext(FitInput{Weights: 26 * gib, VRAM: 48 * gib, Rollback: 7})
	if spec.Ctx >= plain.Ctx || spec.Recurrent != m.RecurrentBytes(0, 7) || plain.Recurrent != m.RecurrentBytes(0, 0) {
		t.Errorf("fit: %d tokens with rollback, %d without; state %d and %d", spec.Ctx, plain.Ctx, spec.Recurrent, plain.Recurrent)
	}
}

func TestSplitFor(t *testing.T) {
	gib := int64(1 << 30)
	got := SplitFor([]int64{24 * gib, 24 * gib}, []int64{0, 4 * gib})
	if got[0] <= got[1] || got[0]-got[1] != 4096 {
		t.Errorf("split %v: GPU 1 must get 4 GiB less", got)
	}
	if got := SplitFor([]int64{gib}, []int64{2 * gib}); got[0] != 0 {
		t.Errorf("a full GPU gets %v", got)
	}
}

func TestPresets(t *testing.T) {
	file := &Meta{Architecture: "qwen4exp", Sampling: Sampling{Temperature: f64(1), TopK: intp(20), TopP: f64(0.95)}}
	for preset, wantTemp := range map[string]float64{"general": 1, "coding": 0.6, "agentic": 0.6, "writing": 1} {
		s, err := PresetSampling(preset, file)
		if err != nil || *s.Temperature != wantTemp || *s.TopK != 20 {
			t.Errorf("%s: %+v %v", preset, s, err)
		}
	}
	if s, _ := PresetSampling("writing", file); *s.MinP != 0.05 {
		t.Errorf("writing min_p %v", *s.MinP)
	}
	// No file values: the family's.
	gemma := &Meta{Architecture: "gemma3"}
	if s, _ := PresetSampling("general", gemma); *s.Temperature != 1 || *s.TopK != 64 {
		t.Errorf("gemma %+v", s)
	}
	if s, src := Recommended(gemma); src != "Gemma 3 recommended settings" || *s.MinP != 0 {
		t.Errorf("gemma recommended %+v %s", s, src)
	}
	// gpt-oss keeps its one setting for every use.
	oss := &Meta{Architecture: "gpt-oss"}
	if s, _ := PresetSampling("coding", oss); *s.Temperature != 1 || *s.TopP != 1 {
		t.Errorf("gpt-oss coding %+v", s)
	}
	// Unknown: llama.cpp defaults, adjusted.
	if s, _ := PresetSampling("coding", &Meta{Architecture: "flint"}); *s.Temperature != 0.6 || *s.TopK != 40 {
		t.Errorf("unknown coding %+v", s)
	}
	if _, err := PresetSampling("poetry", nil); err == nil {
		t.Error("an unknown preset was accepted")
	}
	// The owner's values win over the preset.
	st := Settings{Preset: "coding", Sampling: Sampling{TopK: intp(5)}}
	if s := st.EffectiveSampling(file); *s.TopK != 5 || *s.Temperature != 0.6 {
		t.Errorf("effective %+v", s)
	}
	if err := (Settings{Preset: "poetry"}).Validate(1); err == nil {
		t.Error("Validate accepted an unknown preset")
	}
	if err := (Settings{Fit: "speed"}).Validate(1); err == nil {
		t.Error("Validate accepted an unknown fit")
	}
}
