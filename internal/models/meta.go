package models

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	// GPUWeights is the size of the tensors that llama.cpp puts on the
	// GPUs, over all shards. It leaves out the input layer, which llama.cpp
	// keeps in system RAM. Zero means the tensors could not be read.
	GPUWeights int64 `json:"gpu_weight_bytes,omitempty"`
	// ExpertCount is the number of experts in each MoE layer, or 0 for a
	// dense model.
	ExpertCount int `json:"expert_count,omitempty"`
	// ExpertBytes is the size of the routed expert tensors of each block,
	// by block index. GPUWeights includes them.
	ExpertBytes []int64 `json:"expert_layer_bytes,omitempty"`
}

// Weights returns the bytes of weights on the GPUs: GPUWeights, or the
// file size when the tensors are not known.
func (m Meta) Weights(fileSize int64) int64 {
	if m.GPUWeights > 0 {
		return m.GPUWeights
	}
	return fileSize
}

// HostExperts returns the bytes of experts that the MoE settings keep in
// system RAM: those of the first CPULayers blocks, or of all of them.
func (m Meta) HostExperts(moe MoE) int64 {
	n := len(m.ExpertBytes)
	if moe.CPULayers != AllLayers {
		n = min(n, max(moe.CPULayers, 0))
	}
	var sum int64
	for _, b := range m.ExpertBytes[:n] {
		sum += b
	}
	return sum
}

// VRAMWeights returns the GPU memory for the weights and the MoE cache:
// the weights less the experts in system RAM, plus the cache. llama.cpp
// gives the cache no more slots than the experts in system RAM need.
func (m Meta) VRAMWeights(fileSize int64, moe MoE) int64 {
	host := m.HostExperts(moe)
	return m.Weights(fileSize) - host + min(int64(moe.CacheMiB)<<20, host)
}

// hostTensors are the input layer tensors. llama.cpp keeps them in system
// RAM because it only looks up rows of them. A per-layer embedding table
// can be close to half of a model.
var hostTensors = map[string]bool{
	"token_embd.weight":           true,
	"per_layer_token_embd.weight": true,
	"position_embd.weight":        true,
	"token_types.weight":          true,
}

// expertTensor matches the routed expert tensors of a block. It is
// LLM_FFN_EXPS_REGEX in llama.cpp b11889 (common/common.h), which
// --n-cpu-moe and --cpu-moe use. Shared experts (ffn_*_shexp) stay on
// the GPUs.
var expertTensor = regexp.MustCompile(`^blk\.([0-9]+)\.ffn_(up|down|gate|gate_up)_(ch|)exps`)

// gpuWeights sums the tensors that llama.cpp puts on the GPUs, over the
// shards of the model whose first shard is path, and returns the size of
// the expert tensors of each block. A model with no output.weight uses its
// token embedding as the output layer, and llama.cpp puts a copy of it on
// the GPUs.
func gpuWeights(path string) (int64, []int64, error) {
	var gpu, embd int64
	var experts []int64
	output := false
	for _, p := range shardPaths(path) {
		ts, err := gguf.ReadTensors(p)
		if err != nil {
			return 0, nil, err
		}
		for _, t := range ts {
			switch t.Name {
			case "token_embd.weight":
				embd = t.Size
			case "output.weight":
				output = true
			}
			if !hostTensors[t.Name] {
				gpu += t.Size
			}
			if m := expertTensor.FindStringSubmatch(t.Name); m != nil {
				// block_count bounds the index in a valid file; the
				// limit keeps a corrupt name from allocating.
				if b, err := strconv.Atoi(m[1]); err == nil && b < maxMoELayers {
					for len(experts) <= b {
						experts = append(experts, 0)
					}
					experts[b] += t.Size
				}
			}
		}
	}
	if !output {
		gpu += embd
	}
	return gpu, experts, nil
}

// shardPaths returns every shard of a split model from its first shard,
// or path alone.
func shardPaths(path string) []string {
	dir, name := filepath.Split(path)
	m := splitShard.FindStringSubmatch(name)
	if m == nil {
		return []string{path}
	}
	n, _ := strconv.Atoi(m[3])
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, filepath.Join(dir, fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[3])))
	}
	return out
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
	"attention.recurrent_layers", "full_attention_interval", "attention.sliding_window", "attention.sliding_window_pattern",
	"attention.indexer.head_count", "attention.indexer.key_length",
	"expert_count",
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
	// Without the tensors, the fit counts the whole file as GPU weights.
	if gw, experts, err := gpuWeights(path); err == nil {
		m.GPUWeights, m.ExpertBytes = gw, experts
	}
	return m, nil
}

func metaFrom(arch string, vals map[string]gguf.Value) Meta {
	get := func(k string) gguf.Value { return vals[arch+"."+k] }
	m := Meta{Architecture: arch, ContextLength: get("context_length").Int(), ExpertCount: get("expert_count").Int()}

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
	// Without the list, a hybrid model gives the interval of its full
	// attention layers: with 4, layers 3, 7, 11 and so on keep a cache, as
	// llama.cpp reads it.
	if n := get("full_attention_interval").Int(); recurrent == nil && n > 1 {
		recurrent = make([]float64, layers)
		for i := range recurrent {
			if (i+1)%n != 0 {
				recurrent[i] = 1
			}
		}
	}
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
