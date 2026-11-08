package optimize

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/models"
)

// kvName is a KV cache type as llama.cpp names it: empty is f16.
func kvName(t string) string {
	if t == "" {
		return "f16"
	}
	return t
}

// commonArgs are the flags of one candidate, in the form that every tool
// takes.
func commonArgs(c Candidate) []string {
	var a []string
	if c.SplitMode != "" {
		a = append(a, "-sm", c.SplitMode)
	} else {
		a = append(a, "-sm", "none")
	}
	if c.TensorSplit != "" {
		a = append(a, "-ts", c.TensorSplit)
	}
	if c.Batch > 0 {
		a = append(a, "-b", strconv.Itoa(c.Batch))
	}
	if c.UBatch > 0 {
		a = append(a, "-ub", strconv.Itoa(c.UBatch))
	}
	a = append(a, "-ctk", kvName(c.TypeK), "-ctv", kvName(c.TypeV))
	if c.CPUMoE > 0 {
		a = append(a, "-ncmoe", strconv.Itoa(c.CPUMoE))
	}
	return a
}

// benchArgs builds one llama-bench run for candidates that share a
// backend. llama-bench tests every combination of the comma lists, so the
// run can measure more than the candidates. parseBench keeps only the
// candidates.
func benchArgs(cands []Candidate, prompt, gen, depth, reps int) []string {
	var sm, ts, b, ub, ctk, ctv, moe []string
	add := func(list *[]string, v string) {
		if !slices.Contains(*list, v) {
			*list = append(*list, v)
		}
	}
	for _, c := range cands {
		add(&sm, or(c.SplitMode, "none"))
		if c.TensorSplit != "" {
			add(&ts, c.TensorSplit)
		}
		add(&b, strconv.Itoa(or0(c.Batch, 2048)))
		add(&ub, strconv.Itoa(or0(c.UBatch, 512)))
		add(&ctk, kvName(c.TypeK))
		add(&ctv, kvName(c.TypeV))
		add(&moe, strconv.Itoa(c.CPUMoE))
	}
	a := []string{"-p", strconv.Itoa(prompt), "-n", strconv.Itoa(gen), "-d", strconv.Itoa(depth), "-r", strconv.Itoa(reps),
		"-sm", strings.Join(sm, ","), "-b", strings.Join(b, ","), "-ub", strings.Join(ub, ","),
		"-ctk", strings.Join(ctk, ","), "-ctv", strings.Join(ctv, ","), "-ncmoe", strings.Join(moe, ",")}
	if len(ts) > 0 {
		a = append(a, "-ts", strings.Join(ts, ","))
	}
	return a
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func or0(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// benchRow is one line of llama-bench JSONL output.
type benchRow struct {
	NBatch    int     `json:"n_batch"`
	NUBatch   int     `json:"n_ubatch"`
	TypeK     string  `json:"type_k"`
	TypeV     string  `json:"type_v"`
	NCPUMoE   int     `json:"n_cpu_moe"`
	SplitMode string  `json:"split_mode"`
	NPrompt   int     `json:"n_prompt"`
	NGen      int     `json:"n_gen"`
	AvgTS     float64 `json:"avg_ts"`
	StddevTS  float64 `json:"stddev_ts"`
}

// key is the part of a row that names its settings. The tensor split is
// left out: llama-bench prints it rounded, and one run has one split per
// candidate set.
type benchKey struct {
	batch, ubatch int
	typeK, typeV  string
	moe           int
	split         string
}

func (c Candidate) key() benchKey {
	return benchKey{or0(c.Batch, 2048), or0(c.UBatch, 512), kvName(c.TypeK), kvName(c.TypeV), c.CPUMoE, or(c.SplitMode, "none")}
}

// parseBench reads llama-bench JSONL output, and returns the result of
// each candidate that it measured.
func parseBench(raw []byte, cands []Candidate) ([]Result, error) {
	type pair struct{ pp, tg *benchRow }
	rows := map[benchKey]*pair{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r benchRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("llama-bench output: %w", err)
		}
		k := benchKey{r.NBatch, r.NUBatch, r.TypeK, r.TypeV, r.NCPUMoE, r.SplitMode}
		p := rows[k]
		if p == nil {
			p = &pair{}
			rows[k] = p
		}
		switch {
		case r.NGen > 0:
			p.tg = &r
		case r.NPrompt > 0:
			p.pp = &r
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []Result
	for _, c := range cands {
		p := rows[c.key()]
		if p == nil || p.tg == nil {
			continue
		}
		res := Result{Candidate: c, GenTPS: p.tg.AvgTS, GenStd: p.tg.StddevTS}
		if p.pp != nil {
			res.PromptTPS, res.PromptStd = p.pp.AvgTS, p.pp.StddevTS
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		return nil, errors.New("llama-bench measured none of the settings")
	}
	return out, nil
}

// batchedRow is one line of llama-batched-bench JSONL output.
type batchedRow struct {
	PL      int     `json:"pl"`
	Speed   float64 `json:"speed"`
	SpeedTG float64 `json:"speed_tg"`
}

func parseBatched(raw []byte) ([]batchedRow, error) {
	var out []batchedRow
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r batchedRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("llama-batched-bench output: %w", err)
		}
		if r.PL > 0 {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("llama-batched-bench printed no results")
	}
	return out, nil
}

// fromSettings is the candidate that a model's settings run as.
func fromSettings(s models.Settings, backend string) Candidate {
	c := Candidate{Backend: backend, Batch: s.Batch, UBatch: s.UBatch, TypeK: s.KVCache.TypeK, TypeV: s.KVCache.TypeV,
		SplitMode: s.SplitMode, Parallel: s.Parallel}
	if c.TypeK == "f16" {
		c.TypeK = ""
	}
	if c.TypeV == "f16" {
		c.TypeV = ""
	}
	if s.MoE.CPULayers > 0 {
		c.CPUMoE = s.MoE.CPULayers
	}
	if len(s.TensorSplit) > 0 {
		parts := make([]string, len(s.TensorSplit))
		for i, v := range s.TensorSplit {
			parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		c.TensorSplit = strings.Join(parts, "/")
	}
	return c
}

// apply writes the best candidate into the model's settings, and lists
// what changed from the baseline.
func apply(s models.Settings, from, to Candidate) (models.Settings, []Change) {
	var ch []Change
	set := func(name, a, b string) {
		if a != b {
			ch = append(ch, Change{Setting: name, From: a, To: b})
		}
	}
	num := func(n int) string {
		if n == 0 {
			return "default"
		}
		return strconv.Itoa(n)
	}
	set("batch", num(from.Batch), num(to.Batch))
	set("ubatch", num(from.UBatch), num(to.UBatch))
	set("kv_cache.type_k", kvName(from.TypeK), kvName(to.TypeK))
	set("kv_cache.type_v", kvName(from.TypeV), kvName(to.TypeV))
	set("split_mode", or(from.SplitMode, "layer"), or(to.SplitMode, "layer"))
	set("moe.cpu_layers", num(from.CPUMoE), num(to.CPUMoE))
	set("parallel", num(from.Parallel), num(to.Parallel))
	s.Batch, s.UBatch = to.Batch, to.UBatch
	s.KVCache.TypeK, s.KVCache.TypeV = to.TypeK, to.TypeV
	if to.SplitMode == "layer" {
		to.SplitMode = ""
	}
	s.SplitMode = to.SplitMode
	if s.MoE.CPULayers != models.AllLayers || to.CPUMoE != from.CPUMoE {
		s.MoE.CPULayers = to.CPUMoE
	}
	if to.Parallel > 0 {
		s.Parallel = to.Parallel
	}
	return s, ch
}
