// Package optimize searches llama.cpp settings for the fastest run of one
// model on this host, by measurement. The design is in
// docs/design/optimizer.md.
//
// The search is a fixed sequence of stages. Each stage asks the host agent
// to run one tool once (Runner), and keeps the best result so far. A
// result wins only when it beats the best by more than the noise of the
// measurement. The last stage starts llama-server with the best settings at
// the model's real context, so a recommendation always loads.
package optimize

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/models"
)

// Profile is what the search scores.
type Profile string

// The profiles.
const (
	// Single scores generation speed at a context depth, then prompt speed.
	Single Profile = "single"
	// Throughput scores the total speed of parallel requests, with a floor
	// on the speed of each request.
	Throughput Profile = "throughput"
)

// Tools that the host agent runs.
const (
	ToolBench   = "bench"
	ToolBatched = "batched"
	ToolServer  = "server"
)

// Request is one tool run on the host.
type Request struct {
	Tool    string
	Backend string
	Args    []string
}

// Runner runs a tool on the host and returns its standard output.
type Runner interface {
	Run(ctx context.Context, r Request) ([]byte, error)
}

// Candidate is one set of settings that the search measures.
type Candidate struct {
	Backend     string `json:"backend"`
	SplitMode   string `json:"split_mode,omitempty"`   // layer or row, with more than one GPU
	TensorSplit string `json:"tensor_split,omitempty"` // llama-bench form, such as 3/2
	Batch       int    `json:"batch,omitempty"`
	UBatch      int    `json:"ubatch,omitempty"`
	TypeK       string `json:"type_k,omitempty"` // empty is f16
	TypeV       string `json:"type_v,omitempty"`
	CPUMoE      int    `json:"cpu_moe,omitempty"` // layers with experts in system RAM
	Parallel    int    `json:"parallel,omitempty"`
}

// Result is the measurement of one candidate.
type Result struct {
	Candidate
	Stage     string  `json:"stage"`
	PromptTPS float64 `json:"prompt_tps,omitempty"`
	PromptStd float64 `json:"prompt_std,omitempty"`
	GenTPS    float64 `json:"gen_tps,omitempty"`
	GenStd    float64 `json:"gen_std,omitempty"`
	// TotalTPS and RequestTPS are the throughput profile's numbers at
	// Parallel requests: all tokens per second, and the generation speed
	// of one request.
	TotalTPS   float64 `json:"total_tps,omitempty"`
	RequestTPS float64 `json:"request_tps,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// Input describes the model and the host for one run.
type Input struct {
	Profile Profile
	// Settings are the model's settings now. The recommendation starts
	// from them.
	Settings models.Settings
	// Backend is the backend that llama-swap runs now, and Backends every
	// backend that can run on the selected GPUs.
	Backend  string
	Backends []string
	// GPUs is the number of GPUs that the model runs on.
	GPUs int
	// Ctx is the context size that the model runs with.
	Ctx int
	// KVFits tells whether a KV cache type keeps the context in VRAM.
	KVFits func(typeK, typeV string) bool
	// MoE are the values of CPUMoE to try. Empty skips the stage.
	MoE []int
	// Layers is the model's block count. A model with every expert in
	// system RAM (models.AllLayers) runs with CPUMoE at Layers.
	Layers int
	// Advisor proposes more candidates after the fixed stages. Nil skips
	// the stage.
	Advisor Advisor
}

// Change is one setting that the recommendation changes.
type Change struct {
	Setting string `json:"setting"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Recommendation is the outcome of a run.
type Recommendation struct {
	Baseline Result `json:"baseline"`
	Best     Result `json:"best"`
	// Settings are the model's new settings. Backend is set when a
	// different backend was faster. The backend applies to every model.
	Settings models.Settings `json:"settings"`
	Backend  string          `json:"backend,omitempty"`
	Changes  []Change        `json:"changes"`
	// Verified is true when llama-server loaded the settings at the
	// model's context.
	Verified bool `json:"verified"`
}

// Progress reports the stage and every result so far.
type Progress func(stage string, results []Result)

// Search runs the stages.
type Search struct {
	Runner   Runner
	Progress Progress
	// Repetitions of each llama-bench test. The noise margin needs two.
	Repetitions int
	results     []Result
	in          Input
}

// Default test sizes.
const (
	promptTokens = 512
	genTokens    = 128
	maxDepth     = 8192
)

// depth is the number of tokens in the KV cache when a test runs.
func (in Input) depth() int {
	if in.Ctx > 0 {
		return min(maxDepth, in.Ctx/2)
	}
	return 4096
}

// Run searches, and returns the recommendation.
func (s *Search) Run(ctx context.Context, in Input) (*Recommendation, error) {
	s.in, s.results = in, nil
	if s.Repetitions < 2 {
		s.Repetitions = 3
	}
	if in.Profile != Single && in.Profile != Throughput {
		return nil, fmt.Errorf("profile %q: use %s or %s", in.Profile, Single, Throughput)
	}
	cur := fromSettings(in.Settings, in.Backend)
	if in.Settings.MoE.CPULayers == models.AllLayers {
		cur.CPUMoE = in.Layers
	}
	base, err := s.bench(ctx, "baseline", []Candidate{cur})
	if err != nil {
		return nil, err
	}
	if len(base) == 0 {
		return nil, errors.New("the baseline measurement failed: " + s.lastError())
	}
	baseline := base[0]
	best := baseline
	pick := func(stage string, cands []Candidate) error {
		rs, err := s.bench(ctx, stage, cands)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if better(r, best) {
				best = r
			}
		}
		return nil
	}
	// Backend and split mode.
	var cands []Candidate
	for _, b := range in.Backends {
		for _, sm := range s.splitModes() {
			c := best.Candidate
			c.Backend, c.SplitMode = b, sm
			if c != best.Candidate {
				cands = append(cands, c)
			}
		}
	}
	if err := pick("backend", cands); err != nil {
		return nil, err
	}
	// KV cache.
	cands = nil
	for _, t := range []string{"", "q8_0"} {
		for _, v := range []string{"", "q8_0"} {
			if in.KVFits != nil && !in.KVFits(t, v) {
				continue
			}
			c := best.Candidate
			c.TypeK, c.TypeV = t, v
			if c != best.Candidate {
				cands = append(cands, c)
			}
		}
	}
	if err := pick("kv-cache", cands); err != nil {
		return nil, err
	}
	// Batch sizes.
	cands = nil
	for _, b := range []int{2048, 4096} {
		for _, ub := range []int{256, 512, 1024, 2048} {
			c := best.Candidate
			c.Batch, c.UBatch = b, ub
			if c != best.Candidate {
				cands = append(cands, c)
			}
		}
	}
	if err := pick("batch", cands); err != nil {
		return nil, err
	}
	// MoE experts in system RAM.
	cands = nil
	for _, n := range in.MoE {
		c := best.Candidate
		c.CPUMoE = n
		if c != best.Candidate {
			cands = append(cands, c)
		}
	}
	if err := pick("moe", cands); err != nil {
		return nil, err
	}
	// The advisor's proposals. A failed advisor does not stop the run.
	if in.Advisor != nil {
		s.report("advisor")
		props, err := in.Advisor.Propose(ctx, in, best, slices.Clone(s.results))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			s.results = append(s.results, Result{Candidate: best.Candidate, Stage: "advisor", Error: "advisor: " + err.Error()})
		}
		cands = nil
		for _, p := range props {
			if p != best.Candidate && in.valid(p) && !slices.Contains(cands, p) && len(cands) < maxProposals {
				cands = append(cands, p)
			}
		}
		if err := pick("advisor", cands); err != nil {
			return nil, err
		}
	}
	if in.Profile == Throughput {
		if best, err = s.parallel(ctx, best); err != nil {
			return nil, err
		}
	}
	rec := &Recommendation{Baseline: baseline}
	rec.Best, rec.Verified, err = s.verify(ctx, best, baseline)
	if err != nil {
		return nil, err
	}
	rec.Settings, rec.Changes = apply(in.Settings, baseline.Candidate, rec.Best.Candidate)
	if rec.Best.Backend != in.Backend {
		rec.Backend = rec.Best.Backend
		rec.Changes = append(rec.Changes, Change{Setting: "backend", From: in.Backend, To: rec.Best.Backend})
	}
	s.report("done")
	return rec, nil
}

func (s *Search) splitModes() []string {
	if s.in.GPUs > 1 {
		return []string{"layer", "row"}
	}
	return []string{""}
}

func (s *Search) report(stage string) {
	if s.Progress != nil {
		s.Progress(stage, slices.Clone(s.results))
	}
}

func (s *Search) lastError() string {
	for i := len(s.results) - 1; i >= 0; i-- {
		if e := s.results[i].Error; e != "" {
			return e
		}
	}
	return "no result"
}

// better tells whether a beats b by more than the noise. Generation speed
// decides first. When it is level, prompt speed decides.
func better(a, b Result) bool {
	if a.Error != "" {
		return false
	}
	if a.TotalTPS > 0 || b.TotalTPS > 0 {
		return a.TotalTPS > b.TotalTPS*1.03
	}
	gen := max(3*b.GenStd, 3*a.GenStd, 0.03*b.GenTPS)
	switch {
	case a.GenTPS > b.GenTPS+gen:
		return true
	case a.GenTPS < b.GenTPS-gen:
		return false
	}
	prompt := max(3*b.PromptStd, 3*a.PromptStd, 0.03*b.PromptTPS)
	return a.PromptTPS > b.PromptTPS+prompt
}

// bench measures candidates with llama-bench: one run for each backend,
// with comma lists for the flags that differ.
func (s *Search) bench(ctx context.Context, stage string, cands []Candidate) ([]Result, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	s.report(stage)
	var out []Result
	for _, group := range groupByBackend(cands) {
		args := benchArgs(group, promptTokens, genTokens, s.in.depth(), s.Repetitions)
		raw, err := s.Runner.Run(ctx, Request{Tool: ToolBench, Backend: group[0].Backend, Args: args})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var rs []Result
		if err == nil {
			rs, err = parseBench(raw, group)
		}
		if err != nil {
			for _, c := range group {
				s.results = append(s.results, Result{Candidate: c, Stage: stage, Error: err.Error()})
			}
			continue
		}
		for i := range rs {
			rs[i].Stage = stage
		}
		s.results = append(s.results, rs...)
		out = append(out, rs...)
	}
	s.report(stage)
	return out, nil
}

// parallel finds the number of parallel requests with the most total
// speed, where each request still gets half the speed of one alone.
func (s *Search) parallel(ctx context.Context, best Result) (Result, error) {
	s.report("parallel")
	c := best.Candidate
	args := append(commonArgs(c), "-c", strconv.Itoa(max(s.in.Ctx, promptTokens+genTokens)),
		"-npp", strconv.Itoa(promptTokens), "-ntg", strconv.Itoa(genTokens), "-npl", "1,2,4,8")
	raw, err := s.Runner.Run(ctx, Request{Tool: ToolBatched, Backend: c.Backend, Args: args})
	if ctx.Err() != nil {
		return best, ctx.Err()
	}
	var rows []batchedRow
	if err == nil {
		rows, err = parseBatched(raw)
	}
	if err != nil {
		s.results = append(s.results, Result{Candidate: c, Stage: "parallel", Error: err.Error()})
		s.report("parallel")
		return best, nil
	}
	floor := best.GenTPS / 2
	out := best
	for _, r := range rows {
		res := Result{Candidate: c, Stage: "parallel", PromptTPS: best.PromptTPS, GenTPS: best.GenTPS,
			TotalTPS: r.Speed, RequestTPS: r.SpeedTG / float64(r.PL)}
		res.Parallel = r.PL
		s.results = append(s.results, res)
		if res.RequestTPS >= floor && (out.TotalTPS == 0 || better(res, out)) {
			out = res
		}
	}
	s.report("parallel")
	return out, nil
}

// verify starts llama-server with the best settings at the model's
// context. llama-bench allocates a context only for its test, so a setting
// that it measured can still fail to load at the real context. When the
// best does not load, verify drops the settings that need GPU memory, one
// at a time: the batch sizes, then the KV cache type, then half the
// parallel requests. A variant that was not measured is measured first. At
// last it keeps the baseline.
func (s *Search) verify(ctx context.Context, best, baseline Result) (Result, bool, error) {
	s.report("verify")
	variants := []Candidate{best.Candidate}
	// The measured smaller micro-batches, fastest first.
	var smaller []Result
	for _, r := range s.results {
		q, b := r.Candidate, best.Candidate
		q.Batch, q.UBatch, q.Parallel = b.Batch, b.UBatch, b.Parallel
		if r.Error == "" && r.Stage != "verify" && q == b && or0(r.UBatch, 512) < or0(best.UBatch, 512) {
			smaller = append(smaller, r)
		}
	}
	sort.SliceStable(smaller, func(i, j int) bool { return better(smaller[i], smaller[j]) })
	for _, r := range smaller {
		c := best.Candidate
		c.Batch, c.UBatch = r.Batch, r.UBatch
		variants = append(variants, c)
	}
	c := best.Candidate
	c.Batch, c.UBatch = baseline.Batch, baseline.UBatch
	variants = append(variants, c)
	c.TypeK, c.TypeV = baseline.TypeK, baseline.TypeV
	variants = append(variants, c)
	if c.Parallel > 1 {
		c.Parallel /= 2
		variants = append(variants, c)
	}
	variants = append(variants, baseline.Candidate)
	tried := map[Candidate]bool{}
	for _, v := range variants {
		if tried[v] {
			continue
		}
		tried[v] = true
		r, ok := s.measured(v)
		if !ok {
			rs, err := s.bench(ctx, "verify", []Candidate{v})
			if err != nil {
				return best, false, err
			}
			if len(rs) == 0 {
				continue
			}
			r = rs[0]
			r.Parallel = v.Parallel
		}
		if v != baseline.Candidate && better(baseline, r) {
			continue // the variant is slower than the settings now
		}
		loaded, err := s.load(ctx, v)
		if err != nil {
			return best, false, err
		}
		if loaded {
			s.report("verify")
			return r, true, nil
		}
	}
	return baseline, false, nil
}

// measured finds the result of a candidate that a stage measured.
func (s *Search) measured(c Candidate) (Result, bool) {
	for i := len(s.results) - 1; i >= 0; i-- {
		if r := s.results[i]; r.Error == "" && r.Stage != "verify" && r.Candidate == c {
			return r, true
		}
	}
	// llama-bench does not run parallel requests, so a candidate that
	// differs only in Parallel has the same single-request numbers.
	p := c
	p.Parallel = 0
	for i := len(s.results) - 1; i >= 0; i-- {
		if r := s.results[i]; r.Error == "" && r.Stage != "verify" && r.Stage != "parallel" {
			q := r.Candidate
			q.Parallel = 0
			if q == p {
				r.Candidate = c
				return r, true
			}
		}
	}
	return Result{}, false
}

// load starts llama-server with a candidate at the model's context.
func (s *Search) load(ctx context.Context, c Candidate) (bool, error) {
	args := commonArgs(c)
	if s.in.Ctx > 0 {
		args = append(args, "-c", strconv.Itoa(s.in.Ctx))
	}
	if c.Parallel > 0 {
		args = append(args, "-np", strconv.Itoa(c.Parallel))
	}
	start := time.Now()
	raw, err := s.Runner.Run(ctx, Request{Tool: ToolServer, Backend: c.Backend, Args: args})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil && strings.Contains(string(raw), `"loaded":true`) {
		return true, nil
	}
	msg := "llama-server did not load"
	if err != nil {
		msg = err.Error()
	}
	s.results = append(s.results, Result{Candidate: c, Stage: "verify",
		Error: fmt.Sprintf("%s after %s", msg, time.Since(start).Round(time.Second))})
	s.report("verify")
	return false, nil
}

// groupByBackend splits candidates by backend and tensor split, in order.
// One llama-bench run holds one of each: the backend is the image, and
// llama-bench prints the tensor split rounded, so two splits in one run
// cannot be told apart.
func groupByBackend(cands []Candidate) [][]Candidate {
	var out [][]Candidate
	idx := map[[2]string]int{}
	for _, c := range cands {
		k := [2]string{c.Backend, c.TensorSplit}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], c)
	}
	return out
}
