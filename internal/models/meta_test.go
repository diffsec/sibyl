package models

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ihavespoons/sibyl/internal/gguf"
)

// repeat returns n copies of a pattern, cut to n.
func repeat[T any](pattern []T, n int) []T {
	var out []T
	for len(out) < n {
		out = append(out, pattern...)
	}
	return out[:n]
}

func writeGGUF(t *testing.T, kv map[string]any) string {
	t.Helper()
	data, err := gguf.Encode(kv)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The headers below copy the keys of the models on the test host. The
// expected bytes per token were measured there, from the change in VRAM
// between a 32768 and a 131072 token context (llama.cpp b11889, ROCm):
//
//	Qwen3-Embedding-0.6B          f16 122,900
//	27B hybrid (qwen35)           f16 73,640   q8_0 51,131
//	flint (sliding window)        f16 36,816   q8_0 27,503   q4_0 20,335
//
// The estimate must stay within 1% of each.
func TestBytesPerTokenMatchesMeasurements(t *testing.T) {
	cases := []struct {
		name string
		kv   map[string]any
		want map[string]int64 // cache type -> measured bytes per token
	}{
		{"dense embedding", map[string]any{
			"general.architecture": "qwen3", "qwen3.block_count": 28, "qwen3.context_length": 32768,
			"qwen3.attention.head_count": 16, "qwen3.attention.head_count_kv": 8,
			"qwen3.attention.key_length": 128, "qwen3.attention.value_length": 128,
		}, map[string]int64{"f16": 122900}},
		{"hybrid with one prediction layer", map[string]any{
			"general.architecture": "qwen35", "qwen35.block_count": 65, "qwen35.context_length": 262144,
			"qwen35.nextn_predict_layers": 1, "qwen35.attention.head_count": 24, "qwen35.attention.head_count_kv": 4,
			"qwen35.attention.key_length": 256, "qwen35.attention.value_length": 256,
			"qwen35.attention.recurrent_layers": repeat([]bool{true, true, true, false}, 65),
			"general.sampling.temp":             float32(1), "general.sampling.top_k": 20,
		}, map[string]int64{"f16": 73640, "q8_0": 51131}},
		{"sliding window", map[string]any{
			"general.architecture": "flint", "flint.block_count": 49, "flint.context_length": 262144,
			"flint.attention.head_count": 32, "flint.attention.head_count_kv": 4,
			"flint.attention.key_length": 128, "flint.attention.value_length": 128,
			"flint.attention.sliding_window": uint32(513),
			"flint.attention.sliding_window_pattern": append([]bool{false, false},
				append(repeat([]bool{true, true, true, false}, 46), false)...),
		}, map[string]int64{"f16": 36816, "q8_0": 27503, "q4_0": 20335}},
	}
	for _, c := range cases {
		m, err := ReadMeta(writeGGUF(t, c.kv))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for typ, want := range c.want {
			got := m.BytesPerToken(typ, typ)
			if d := float64(got-want) / float64(want); d > 0.01 || d < -0.01 {
				t.Errorf("%s %s: %d bytes per token, measured %d", c.name, typ, got, want)
			}
		}
	}
}

func TestReadMetaSamplingAndNativeContext(t *testing.T) {
	m, err := ReadMeta(writeGGUF(t, map[string]any{
		"general.architecture": "llama", "llama.block_count": 2, "llama.context_length": 8192,
		"llama.embedding_length": 4096, "llama.attention.head_count": 32, "llama.attention.head_count_kv": 8,
		"general.sampling.temp": float32(0.7), "general.sampling.top_k": 20,
		"tokenizer.ggml.tokens": []string{"a", "b"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextLength != 8192 || m.AttentionLayers != 2 {
		t.Errorf("meta = %+v", m)
	}
	// key_length falls back to embedding_length / head_count = 128.
	if m.KElems != 2*8*128 {
		t.Errorf("K elements = %d", m.KElems)
	}
	if m.Sampling.TopK == nil || *m.Sampling.TopK != 20 || m.Sampling.Temperature == nil {
		t.Errorf("sampling = %+v", m.Sampling)
	}
	if m.ContextBytes(0, "", "") != 8192*m.BytesPerToken("", "") {
		t.Error("a context of 0 must use the native context")
	}
}

func TestReadMetaRefusesNonGGUF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.gguf")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(p); err == nil {
		t.Error("want an error for a file that is not GGUF")
	}
}
