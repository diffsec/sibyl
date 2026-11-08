package optimize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Advisor proposes candidates from the measurements so far.
type Advisor interface {
	Propose(ctx context.Context, in Input, best Result, results []Result) ([]Candidate, error)
}

// maxProposals is the most advisor candidates that the search measures.
const maxProposals = 6

// Limits of a candidate. The host agent's tune allowlist holds the same.
const (
	minBatch    = 32
	maxBatch    = 65536
	maxParallel = 64
)

var (
	kvTypes     = []string{"", "f16", "q8_0", "q4_0"}
	tensorSplit = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3})?(/[0-9]{1,3}(\.[0-9]{1,3})?){0,15}$`)
)

// valid checks a candidate against the host and the limits. The search
// measures only valid candidates, whatever an advisor proposes.
func (in Input) valid(c Candidate) bool {
	backends := in.Backends
	if len(backends) == 0 {
		backends = []string{in.Backend}
	}
	if !slices.Contains(backends, c.Backend) {
		return false
	}
	switch c.SplitMode {
	case "":
	case "layer", "row":
		if in.GPUs < 2 {
			return false
		}
	default:
		return false
	}
	if c.TensorSplit != "" && (in.GPUs < 2 || !tensorSplit.MatchString(c.TensorSplit) ||
		strings.Count(c.TensorSplit, "/")+1 != in.GPUs) {
		return false
	}
	for _, b := range []int{c.Batch, c.UBatch} {
		if b != 0 && (b < minBatch || b > maxBatch) {
			return false
		}
	}
	if c.Batch > 0 && c.UBatch > c.Batch {
		return false
	}
	if !slices.Contains(kvTypes, c.TypeK) || !slices.Contains(kvTypes, c.TypeV) {
		return false
	}
	if in.KVFits != nil && !in.KVFits(strings.TrimPrefix(c.TypeK, "f16"), strings.TrimPrefix(c.TypeV, "f16")) {
		return false
	}
	if c.CPUMoE < 0 || c.CPUMoE > in.Layers {
		return false
	}
	if c.Parallel < 0 || c.Parallel > maxParallel || (in.Profile != Throughput && c.Parallel > 0) {
		return false
	}
	return true
}

// LLMAdvisor asks an upstream model for candidates. Ask sends one question
// and returns the answer. Facts describe the hardware and the model, with
// no prompts and no user data.
type LLMAdvisor struct {
	Ask   func(ctx context.Context, system, prompt string) (string, error)
	Facts string
}

// advisorSystem tells the model what to answer.
const advisorSystem = `You tune llama.cpp inference for one model on one machine. You see the hardware, the model, and every setting that was measured with llama-bench, with its speed. Propose up to 6 new settings that are likely to be faster. Do not repeat a measured setting.

You may set only these fields. Leave a field out to keep the best setting's value.
- "backend": one of the backends listed under the hardware
- "split_mode": "layer" or "row", only with more than one GPU
- "tensor_split": the share of layers on each GPU, such as "3/2", only with more than one GPU
- "batch" and "ubatch": 32 to 65536, ubatch not larger than batch
- "type_k" and "type_v": "f16", "q8_0" or "q4_0"
- "cpu_moe": the number of layers whose MoE experts stay in system RAM, 0 to the layer count, only for a MoE model
- "parallel": 1 to 64 parallel requests, only for the throughput profile

Answer with JSON only, and nothing after it: {"candidates": [{"ubatch": 1024, "split_mode": "row"}, ...], "reason": "one sentence"}`

// Propose asks the model, and turns its answer into candidates from the
// best setting.
func (a LLMAdvisor) Propose(ctx context.Context, in Input, best Result, results []Result) ([]Candidate, error) {
	var b strings.Builder
	b.WriteString(a.Facts)
	fmt.Fprintf(&b, "\nProfile: %s. Backends: %s. GPUs for the model: %d. MoE layers: %d. Context: %d tokens.\n",
		in.Profile, strings.Join(in.Backends, ", "), in.GPUs, in.Layers, in.Ctx)
	fmt.Fprintf(&b, "Each test: %d prompt tokens and %d generated tokens, after %d tokens in the KV cache.\n\n",
		promptTokens, genTokens, in.depth())
	b.WriteString("Measured settings (gen and prompt in tokens per second):\n")
	for _, r := range results {
		c, _ := json.Marshal(r.Candidate)
		if r.Error != "" {
			fmt.Fprintf(&b, "- %s %s: failed: %.200s\n", r.Stage, c, r.Error)
			continue
		}
		fmt.Fprintf(&b, "- %s %s: gen %.1f, prompt %.0f", r.Stage, c, r.GenTPS, r.PromptTPS)
		if r.TotalTPS > 0 {
			fmt.Fprintf(&b, ", total %.1f over %d requests", r.TotalTPS, r.Parallel)
		}
		b.WriteString("\n")
	}
	bc, _ := json.Marshal(best.Candidate)
	fmt.Fprintf(&b, "\nBest so far: %s\n", bc)
	answer, err := a.Ask(ctx, advisorSystem, b.String())
	if err != nil {
		return nil, err
	}
	return parseProposals(answer, best.Candidate)
}

// parseProposals reads the advisor's JSON. Each candidate starts from the
// best setting, and changes the fields that it names.
func parseProposals(answer string, best Candidate) ([]Candidate, error) {
	i, j := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if i < 0 || j < i {
		return nil, errors.New("the advisor's answer has no JSON")
	}
	var raw struct {
		Candidates []json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(answer[i:j+1]), &raw); err != nil {
		return nil, fmt.Errorf("the advisor's answer: %w", err)
	}
	var out []Candidate
	for _, r := range raw.Candidates {
		c := best
		// Unknown fields fail, so a proposal cannot carry a flag that the
		// search does not know.
		dec := json.NewDecoder(strings.NewReader(string(r)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			continue
		}
		if c.TypeK == "f16" {
			c.TypeK = ""
		}
		if c.TypeV == "f16" {
			c.TypeV = ""
		}
		out = append(out, c)
	}
	return out, nil
}
