package models

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/diffsec/sibyl/internal/gguf"
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
		// The same model with only the interval, as some merges write it:
		// the 16 full attention layers give the same cost.
		{"hybrid with an interval", map[string]any{
			"general.architecture": "qwen35", "qwen35.block_count": 64, "qwen35.context_length": 262144,
			"qwen35.attention.head_count": 24, "qwen35.attention.head_count_kv": 4,
			"qwen35.attention.key_length": 256, "qwen35.attention.value_length": 256,
			"qwen35.full_attention_interval": uint32(4),
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

func TestReadMetaGPUWeights(t *testing.T) {
	kv := map[string]any{"general.architecture": "llama", "llama.context_length": uint32(4096)}
	write := func(dir, name string, ts []gguf.Tensor) string {
		data, err := gguf.EncodeTensors(kv, ts)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A split model with a per-layer embedding table and its own output
	// layer: both embedding tables stay in system RAM.
	dir := t.TempDir()
	first := write(dir, "m-00001-of-00002.gguf", []gguf.Tensor{
		{Name: "token_embd.weight", Size: 1024}, {Name: "per_layer_token_embd.weight", Size: 4096},
		{Name: "blk.0.ffn_up.weight", Size: 512},
	})
	write(dir, "m-00002-of-00002.gguf", []gguf.Tensor{{Name: "output.weight", Size: 1024}})
	m, err := ReadMeta(first)
	if err != nil {
		t.Fatal(err)
	}
	if m.GPUWeights != 512+1024 {
		t.Errorf("split model: GPU weights = %d, want %d", m.GPUWeights, 512+1024)
	}
	if m.Weights(1<<40) != m.GPUWeights {
		t.Error("Weights did not use the GPU weights")
	}

	// A tied output layer: llama.cpp copies the token embedding to the GPUs.
	tied := write(t.TempDir(), "t.gguf", []gguf.Tensor{
		{Name: "token_embd.weight", Size: 1024}, {Name: "blk.0.ffn_up.weight", Size: 512},
	})
	if m, err := ReadMeta(tied); err != nil || m.GPUWeights != 1024+512 {
		t.Errorf("tied output: GPU weights = %d, %v", m.GPUWeights, err)
	}

	// A missing second shard leaves the file size in use.
	alone := write(t.TempDir(), "a-00001-of-00002.gguf", []gguf.Tensor{{Name: "blk.0.ffn_up.weight", Size: 512}})
	if m, err := ReadMeta(alone); err != nil || m.GPUWeights != 0 || m.Weights(777) != 777 {
		t.Errorf("missing shard: GPU weights = %d, %v", m.GPUWeights, err)
	}
}

func TestReadMetaExperts(t *testing.T) {
	kv := map[string]any{"general.architecture": "qwen3moe", "qwen3moe.context_length": uint32(4096),
		"qwen3moe.expert_count": uint32(128)}
	data, err := gguf.EncodeTensors(kv, []gguf.Tensor{
		{Name: "token_embd.weight", Size: 1024}, {Name: "output.weight", Size: 1024},
		{Name: "blk.0.attn_q.weight", Size: 96},
		{Name: "blk.0.ffn_up_exps.weight", Size: 1024}, {Name: "blk.0.ffn_down_exps.weight", Size: 1024},
		{Name: "blk.0.ffn_up_shexp.weight", Size: 64}, // shared expert: stays on the GPUs
		{Name: "blk.1.ffn_gate_up_exps.weight", Size: 3072},
		{Name: "blk.2.ffn_gate_chexps.weight", Size: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "moe.gguf")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ExpertCount != 128 {
		t.Errorf("expert count = %d, want 128", m.ExpertCount)
	}
	if want := []int64{2048, 3072, 512}; !slices.Equal(m.ExpertBytes, want) {
		t.Errorf("expert bytes = %v, want %v", m.ExpertBytes, want)
	}
	gpu := int64(1024 + 96 + 2048 + 64 + 3072 + 512)
	if m.GPUWeights != gpu {
		t.Fatalf("GPU weights = %d, want %d", m.GPUWeights, gpu)
	}

	cases := []struct {
		moe  MoE
		want int64
	}{
		{MoE{}, gpu},
		{MoE{CPULayers: 1}, gpu - 2048},
		{MoE{CPULayers: 99}, gpu - 5632}, // more layers than the model has
		{MoE{CPULayers: AllLayers}, gpu - 5632},
		// The cache comes back as GPU memory, up to the experts it can hold.
		{MoE{CPULayers: AllLayers, CacheMiB: 1}, gpu},
	}
	for _, c := range cases {
		if got := m.VRAMWeights(0, c.moe); got != c.want {
			t.Errorf("%+v: VRAM weights = %d, want %d", c.moe, got, c.want)
		}
	}

	// A cache smaller than the host experts counts in full.
	big := Meta{GPUWeights: 10 << 30, ExpertBytes: []int64{4 << 30, 4 << 30}}
	if got, want := big.VRAMWeights(0, MoE{CPULayers: AllLayers, CacheMiB: 1024}), int64(2<<30+1<<30); got != want {
		t.Errorf("cache 1 GiB: VRAM weights = %d, want %d", got, want)
	}
}
