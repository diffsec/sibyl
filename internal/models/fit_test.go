package models

import "testing"

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
		f := m.FitContext(c.mode, c.weights, c.vram, c.ctx, c.k, c.v)
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
	if f := small.FitContext(FitContext, 1*gib, 2*gib, 0, "", ""); f.TypeK != "" {
		t.Errorf("small model: %+v", f)
	}
	if f := m.FitContext(FitQuality, 1, 0, 0, "", ""); f.Auto || f.Ctx != 0 {
		t.Errorf("unknown VRAM: %+v", f)
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
