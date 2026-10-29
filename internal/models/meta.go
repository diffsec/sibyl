package models

import (
	"slices"
	"strings"

	"github.com/diffsec/sibyl/internal/gguf"
)

// Meta is what the model file says about its context and the KV cache. The
// System API reads it from the GGUF header of the first shard.
type Meta struct {
	Architecture string `json:"architecture"`
	// Name is general.name, such as "Qwen3 32B", for the model family.
	Name string `json:"name,omitempty"`
	// ContextLength is the native context, in tokens. llama-server uses it
	// when no context size is set.
	ContextLength int `json:"context_length,omitempty"`
	// KElems and VElems are the K and V cache elements per token, summed
	// over the layers that keep a full-length cache.
	KElems int64 `json:"kv_k_elements"`
	VElems int64 `json:"kv_v_elements"`
	// MaxLayerKV is the largest K plus V element count of one layer. A
	// quantized cache needs an f32 buffer of that size per token.
	MaxLayerKV int64 `json:"kv_max_layer_elements"`
	// IndexerElems are the per-token elements of a sparse attention
	// indexer, which llama.cpp keeps in f16.
	IndexerElems int64 `json:"indexer_elements,omitempty"`
	// AttentionLayers is the number of layers counted above.
	AttentionLayers int `json:"attention_layers"`
	// Sampling holds the defaults that the file recommends. llama-server
	// applies them when a setting is not given.
	Sampling Sampling `json:"sampling,omitzero"`
}

// computePerToken is llama.cpp's compute buffer growth per context token
// with flash attention and the default batch sizes. It was measured on the
// test host (llama.cpp b11889, ROCm, two RX 7900 XTX) on four models, and
// matched within 0.4% on three of them.
const computePerToken = 8192

// cacheBytes are the bytes per element of each KV cache type: ggml stores
// the quantized types in blocks of 32 elements.
var cacheBytes = map[string]float64{
	"f32": 4, "f16": 2, "bf16": 2,
	"q8_0": 34.0 / 32, "q4_0": 18.0 / 32, "q4_1": 20.0 / 32,
	"iq4_nl": 18.0 / 32, "q5_0": 22.0 / 32, "q5_1": 24.0 / 32,
}

func quantized(t string) bool { return t != "" && t != "f32" && t != "f16" && t != "bf16" }

// BytesPerToken estimates the GPU memory that one token of context costs:
// the KV cache plus the compute buffers that grow with the context. An
// empty cache type is f16, llama-server's default.
func (m Meta) BytesPerToken(typeK, typeV string) int64 {
	bk, ok := cacheBytes[typeK]
	if !ok {
		bk = 2
	}
	bv, ok := cacheBytes[typeV]
	if !ok {
		bv = 2
	}
	per := float64(m.KElems)*bk + float64(m.VElems)*bv + float64(m.IndexerElems)*2 + computePerToken
	if quantized(typeK) || quantized(typeV) {
		per += 4 * float64(m.MaxLayerKV)
	}
	return int64(per)
}

// ContextBytes estimates the GPU memory for ctx tokens of context. A ctx of
// 0 means the native context.
func (m Meta) ContextBytes(ctx int, typeK, typeV string) int64 {
	if ctx <= 0 {
		ctx = m.ContextLength
	}
	return int64(ctx) * m.BytesPerToken(typeK, typeV)
}

// archKeys are the architecture keys that ReadMeta uses, without the
// "<architecture>." prefix.
var archKeys = []string{
	"context_length", "block_count", "nextn_predict_layers", "embedding_length",
	"attention.head_count", "attention.head_count_kv", "attention.key_length", "attention.value_length",
	"attention.recurrent_layers", "attention.sliding_window", "attention.sliding_window_pattern",
	"attention.indexer.head_count", "attention.indexer.key_length",
}

// ReadMeta reads the GGUF header of a model file.
func ReadMeta(path string) (Meta, error) {
	vals, err := gguf.ReadFile(path, func(k string) bool {
		if k == "general.architecture" || k == "general.name" || strings.HasPrefix(k, "general.sampling.") {
			return true
		}
		// Architecture names have no dot, so the rest of the key follows
		// the first one.
		_, rest, ok := strings.Cut(k, ".")
		return ok && slices.Contains(archKeys, rest)
	})
	if err != nil {
		return Meta{}, err
	}
	m := metaFrom(vals["general.architecture"].Str, vals)
	m.Name = vals["general.name"].Str
	return m, nil
}

func metaFrom(arch string, vals map[string]gguf.Value) Meta {
	get := func(k string) gguf.Value { return vals[arch+"."+k] }
	m := Meta{Architecture: arch, ContextLength: get("context_length").Int()}

	layers := get("block_count").Int()
	// The last nextn_predict_layers blocks are multi-token prediction
	// layers, which llama-server loads only for draft-mtp.
	layers -= get("nextn_predict_layers").Int()
	if layers <= 0 {
		return m
	}
	heads := get("attention.head_count").Num
	embd := get("embedding_length").Num
	kLen, vLen := get("attention.key_length").Num, get("attention.value_length").Num
	if kLen == 0 && heads > 0 {
		kLen = embd / heads
	}
	if vLen == 0 {
		vLen = kLen
	}
	headsKV := perLayer(get("attention.head_count_kv"), layers)
	if headsKV == nil {
		headsKV = perLayer(get("attention.head_count"), layers)
	}
	recurrent := perLayer(get("attention.recurrent_layers"), layers)
	// A sliding-window layer keeps only the window, so its cache does not
	// grow with the context.
	swa := perLayer(get("attention.sliding_window_pattern"), layers)
	if get("attention.sliding_window").Num == 0 {
		swa = nil
	}
	idx := get("attention.indexer.head_count").Num * get("attention.indexer.key_length").Num

	for i := 0; i < layers; i++ {
		if (recurrent != nil && recurrent[i] != 0) || (swa != nil && swa[i] != 0) {
			continue
		}
		h := 0.0
		if headsKV != nil {
			h = headsKV[i]
		}
		if h == 0 {
			continue
		}
		k, v := int64(h*kLen), int64(h*vLen)
		m.KElems += k
		m.VElems += v
		m.MaxLayerKV = max(m.MaxLayerKV, k+v)
		m.IndexerElems += int64(idx)
		m.AttentionLayers++
	}

	s := func(k string) *float64 {
		if v, ok := vals["general.sampling."+k]; ok && !v.IsArr {
			n := v.Num
			return &n
		}
		return nil
	}
	m.Sampling.Temperature, m.Sampling.TopP, m.Sampling.MinP = s("temp"), s("top_p"), s("min_p")
	if v := s("top_k"); v != nil {
		n := int(*v)
		m.Sampling.TopK = &n
	}
	return m
}

// perLayer turns a scalar or a per-layer array into one value per layer.
// It returns nil when the key is missing.
func perLayer(v gguf.Value, layers int) []float64 {
	if v.IsArr {
		if len(v.Array) == 0 {
			return nil
		}
		out := slices.Clone(v.Array)
		for len(out) < layers {
			out = append(out, out[len(out)-1])
		}
		return out[:layers]
	}
	if v.Num == 0 {
		return nil
	}
	out := make([]float64, layers)
	for i := range out {
		out[i] = v.Num
	}
	return out
}
