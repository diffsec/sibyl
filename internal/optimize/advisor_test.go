package optimize

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fixedAdvisor struct {
	props []Candidate
	err   error
	seen  []Result
}

func (a *fixedAdvisor) Propose(_ context.Context, _ Input, _ Result, rs []Result) ([]Candidate, error) {
	a.seen = rs
	return a.props, a.err
}

func TestSearchAdvisor(t *testing.T) {
	h := &fakeHost{}
	adv := &fixedAdvisor{props: []Candidate{
		{Backend: "hip", SplitMode: "row", UBatch: 1024}, // valid: row split is 10 faster
		{Backend: "cuda"},                          // not on this host
		{Backend: "hip", Batch: 512, UBatch: 1024}, // ubatch over batch
		{Backend: "hip", Parallel: 4},              // parallel in the single profile
		{Backend: "hip", TensorSplit: "1/1/1"},     // three values for two GPUs
	}}
	rec, err := (&Search{Runner: h}).Run(context.Background(), Input{
		Profile: Single, Backend: "hip", Backends: []string{"hip"}, GPUs: 2, Ctx: 8192, Advisor: adv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(adv.seen) == 0 {
		t.Error("the advisor saw no results")
	}
	var measured int
	for _, c := range h.calls {
		if c.Tool == ToolBench && strings.Contains(strings.Join(c.Args, " "), "-ub 1024") && values(c.Args)["-sm"][0] == "row" {
			measured++
		}
	}
	if measured == 0 {
		t.Error("the valid proposal was not measured")
	}
	for _, c := range h.calls {
		if c.Backend == "cuda" || strings.Contains(strings.Join(c.Args, " "), "-b 512") {
			t.Errorf("an invalid proposal was measured: %+v", c)
		}
	}
	if rec.Best.SplitMode != "row" {
		t.Errorf("best %+v", rec.Best.Candidate)
	}
	// A failed advisor does not stop the run.
	if _, err := (&Search{Runner: &fakeHost{}}).Run(context.Background(), Input{
		Profile: Single, Backend: "hip", Backends: []string{"hip"}, GPUs: 1, Ctx: 8192,
		Advisor: &fixedAdvisor{err: errors.New("provider down")},
	}); err != nil {
		t.Errorf("err %v", err)
	}
}

func TestValidCandidate(t *testing.T) {
	in := Input{Profile: Throughput, Backend: "hip", Backends: []string{"hip", "vulkan"}, GPUs: 2, Layers: 40,
		KVFits: func(k, v string) bool { return k != "q4_0" }}
	for _, c := range []struct {
		c    Candidate
		want bool
	}{
		{Candidate{Backend: "vulkan", SplitMode: "row", TensorSplit: "3/2", Batch: 4096, UBatch: 1024, TypeK: "q8_0", CPUMoE: 10, Parallel: 8}, true},
		{Candidate{Backend: "rocm"}, false},
		{Candidate{Backend: "hip", SplitMode: "tensor"}, false},
		{Candidate{Backend: "hip", TensorSplit: "3/2/1"}, false},
		{Candidate{Backend: "hip", TensorSplit: "3,2"}, false},
		{Candidate{Backend: "hip", Batch: 16}, false},
		{Candidate{Backend: "hip", UBatch: 100000}, false},
		{Candidate{Backend: "hip", TypeK: "q4_0"}, false}, // does not fit
		{Candidate{Backend: "hip", TypeV: "f32"}, false},
		{Candidate{Backend: "hip", CPUMoE: 41}, false},
		{Candidate{Backend: "hip", Parallel: 65}, false},
	} {
		if got := in.valid(c.c); got != c.want {
			t.Errorf("%+v: %v", c.c, got)
		}
	}
	one := Input{Profile: Single, Backend: "hip", GPUs: 1}
	if one.valid(Candidate{Backend: "hip", SplitMode: "row"}) || one.valid(Candidate{Backend: "hip", Parallel: 2}) {
		t.Error("a split on one GPU or parallel in the single profile passed")
	}
}

func TestLLMAdvisor(t *testing.T) {
	var gotSystem, gotPrompt string
	a := LLMAdvisor{Facts: "GPUs: 2x Radeon RX 7900 XTX, 24 GiB each.", Ask: func(_ context.Context, system, prompt string) (string, error) {
		gotSystem, gotPrompt = system, prompt
		return "Here you go:\n```json\n" + `{"candidates":[{"ubatch":1024,"split_mode":"row"},{"type_k":"f16","cpu_moe":4},{"ubatch":512,"rpc":"10.0.0.1"}],"reason":"r"}` + "\n```", nil
	}}
	best := Result{Candidate: Candidate{Backend: "hip", Batch: 2048}, GenTPS: 50}
	got, err := a.Propose(context.Background(), Input{Profile: Single, Backends: []string{"hip"}, GPUs: 2, Ctx: 8192},
		best, []Result{{Candidate: best.Candidate, Stage: "baseline", GenTPS: 50, PromptTPS: 1000}, {Stage: "batch", Error: "oom"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UBatch != 1024 || got[0].SplitMode != "row" || got[0].Batch != 2048 || got[1].TypeK != "" || got[1].CPUMoE != 4 {
		t.Errorf("candidates %+v", got)
	}
	for _, want := range []string{"7900 XTX", "gen 50.0, prompt 1000", "failed: oom", "Best so far"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("no %q in the prompt:\n%s", want, gotPrompt)
		}
	}
	if !strings.Contains(gotSystem, "JSON only") {
		t.Error("the system prompt asks for no JSON")
	}
	if _, err := parseProposals("no json here", best.Candidate); err == nil {
		t.Error("an answer with no JSON passed")
	}
}
