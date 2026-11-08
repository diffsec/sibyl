package optimize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/models"
)

// fakeHost answers tool runs from a model of a GPU:
//   - generation: 50 tok/s, +5 on vulkan, +10 with row split, -2 per MoE
//     layer in system RAM, -1 with a q8_0 cache (inside the noise);
//   - prompt: 2000 tok/s, +600 at ubatch 1024, +700 at 2048;
//   - llama-server does not load with ubatch 2048 (the compute buffer).
type fakeHost struct {
	calls []Request
	fail  map[string]bool // tools that fail
}

func values(args []string) map[string][]string {
	m := map[string][]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[args[i]] = strings.Split(args[i+1], ",")
	}
	return m
}

func (h *fakeHost) gen(backend, sm, ctk string, moe int) float64 {
	g := 50.0
	if backend == "vulkan" {
		g += 5
	}
	if sm == "row" {
		g += 10
	}
	if ctk == "q8_0" {
		g--
	}
	return g - 2*float64(moe)
}

func (h *fakeHost) Run(_ context.Context, r Request) ([]byte, error) {
	h.calls = append(h.calls, r)
	if h.fail[r.Tool] {
		return nil, errors.New(r.Tool + " failed")
	}
	v := values(r.Args)
	var b strings.Builder
	switch r.Tool {
	case ToolBench:
		for _, sm := range v["-sm"] {
			for _, bs := range v["-b"] {
				for _, ub := range v["-ub"] {
					for _, ctk := range v["-ctk"] {
						for _, ctv := range v["-ctv"] {
							for _, moe := range v["-ncmoe"] {
								n, _ := strconv.Atoi(moe)
								bn, _ := strconv.Atoi(bs)
								ubn, _ := strconv.Atoi(ub)
								pp := 2000.0
								switch ubn {
								case 1024:
									pp += 600
								case 2048:
									pp += 700
								}
								row := map[string]any{"n_batch": bn, "n_ubatch": ubn, "type_k": ctk, "type_v": ctv, "n_cpu_moe": n, "split_mode": sm}
								for _, test := range []struct {
									np, ng int
									ts     float64
								}{{512, 0, pp}, {0, 128, h.gen(r.Backend, sm, ctk, n)}} {
									row["n_prompt"], row["n_gen"], row["avg_ts"], row["stddev_ts"] = test.np, test.ng, test.ts, 0.2
									j, _ := json.Marshal(row)
									b.Write(j)
									b.WriteString("\n")
								}
							}
						}
					}
				}
			}
		}
	case ToolBatched:
		for _, pl := range []int{1, 2, 4, 8} {
			// More requests give more total speed, and each one gets slower.
			tg := 60 * float64(pl) / (1 + 0.3*float64(pl-1))
			fmt.Fprintf(&b, `{"pl":%d,"speed":%f,"speed_tg":%f}`+"\n", pl, tg*1.2, tg)
		}
	case ToolServer:
		if v["-ub"] != nil && v["-ub"][0] == "2048" {
			return []byte(`{"loaded":false,"seconds":3}`), nil
		}
		return []byte(`{"loaded":true,"seconds":12}`), nil
	}
	return []byte(b.String()), nil
}

func TestSearchSingle(t *testing.T) {
	h := &fakeHost{}
	var stages []string
	s := &Search{Runner: h, Progress: func(stage string, _ []Result) {
		if len(stages) == 0 || stages[len(stages)-1] != stage {
			stages = append(stages, stage)
		}
	}}
	rec, err := s.Run(context.Background(), Input{
		Profile: Single, Settings: models.Settings{CtxSize: 65536, Args: []string{"--mlock"}},
		Backend: "hip", Backends: []string{"hip", "vulkan"}, GPUs: 2, Ctx: 65536,
		KVFits: func(k, v string) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	b := rec.Best
	// vulkan with row split is fastest. q8_0 is inside the noise, so it
	// stays f16. ubatch 2048 is faster but does not load, so 1024 wins.
	if b.Backend != "vulkan" || b.SplitMode != "row" || b.TypeK != "" || b.UBatch != 1024 || !rec.Verified {
		t.Errorf("best %+v verified %v", b.Candidate, rec.Verified)
	}
	if rec.Backend != "vulkan" || rec.Settings.SplitMode != "row" || rec.Settings.UBatch != 1024 ||
		rec.Settings.CtxSize != 65536 || rec.Settings.Args[0] != "--mlock" {
		t.Errorf("recommendation %+v", rec)
	}
	if rec.Baseline.GenTPS != 50 || rec.Best.GenTPS != 65 {
		t.Errorf("baseline %v best %v", rec.Baseline.GenTPS, rec.Best.GenTPS)
	}
	want := "baseline backend kv-cache batch verify done"
	if got := strings.Join(stages, " "); got != want {
		t.Errorf("stages %q", got)
	}
	// Every bench run sets the depth: half the context, at most 8192.
	for _, c := range h.calls {
		if c.Tool == ToolBench && values(c.Args)["-d"][0] != "8192" {
			t.Errorf("depth %v", c.Args)
		}
	}
	// The verify of ubatch 2048 failed, and that is in the results.
	var failed bool
	for _, r := range s.results {
		failed = failed || (r.Stage == "verify" && r.UBatch == 2048 && r.Error != "")
	}
	if !failed {
		t.Error("the failed verify is not in the results")
	}
}

func TestSearchKVFitAndMoE(t *testing.T) {
	h := &fakeHost{}
	rec, err := (&Search{Runner: h}).Run(context.Background(), Input{
		Profile: Single, Settings: models.Settings{MoE: models.MoE{CPULayers: models.AllLayers}},
		Backend: "hip", Backends: []string{"hip"}, GPUs: 1, Ctx: 8192, Layers: 40,
		KVFits: func(k, v string) bool { return k == "q8_0" && v == "q8_0" }, // only q8_0 fits
		MoE:    []int{10, 12, 14},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The baseline ran with all 40 layers in system RAM.
	first := values(h.calls[0].Args)
	if first["-ncmoe"][0] != "40" || first["-d"][0] != "4096" || first["-sm"][0] != "none" {
		t.Errorf("baseline args %v", h.calls[0].Args)
	}
	for _, c := range h.calls {
		if v := values(c.Args); c.Tool == ToolBench && strings.Contains(strings.Join(v["-ctk"], ","), "f16") &&
			strings.Contains(strings.Join(v["-ctv"], ","), "q8_0") {
			t.Errorf("a KV type that does not fit was measured: %v", c.Args)
		}
	}
	if rec.Best.CPUMoE != 10 || rec.Settings.MoE.CPULayers != 10 {
		t.Errorf("best %+v settings %+v", rec.Best.Candidate, rec.Settings.MoE)
	}
}

func TestSearchThroughput(t *testing.T) {
	h := &fakeHost{}
	rec, err := (&Search{Runner: h}).Run(context.Background(), Input{
		Profile: Throughput, Backend: "hip", Backends: []string{"hip"}, GPUs: 1, Ctx: 32768,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Per request at 8 is 60*8/3.1/8 = 19.4, under half of 50. At 4 it is
	// 60*4/1.9/4 = 31.6, so 4 wins.
	if rec.Best.Parallel != 4 || rec.Settings.Parallel != 4 {
		t.Errorf("best %+v", rec.Best)
	}
	last := h.calls[len(h.calls)-1]
	if last.Tool != ToolServer || values(last.Args)["-np"][0] != "4" || values(last.Args)["-c"][0] != "32768" {
		t.Errorf("verify %v", last)
	}
}

func TestSearchFailures(t *testing.T) {
	// A bench that fails at the baseline stops the run.
	if _, err := (&Search{Runner: &fakeHost{fail: map[string]bool{ToolBench: true}}}).Run(context.Background(),
		Input{Profile: Single, Backend: "hip"}); err == nil || !strings.Contains(err.Error(), "bench failed") {
		t.Errorf("err %v", err)
	}
	// A server that never loads keeps the baseline, not verified.
	rec, err := (&Search{Runner: &fakeHost{fail: map[string]bool{ToolServer: true}}}).Run(context.Background(),
		Input{Profile: Single, Backend: "hip", Backends: []string{"hip"}, GPUs: 1, Ctx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verified || len(rec.Changes) != 0 {
		t.Errorf("recommendation %+v", rec)
	}
	if _, err := (&Search{Runner: &fakeHost{}}).Run(context.Background(), Input{Profile: "fast"}); err == nil {
		t.Error("a bad profile passed")
	}
}

func TestBetterNoise(t *testing.T) {
	base := Result{GenTPS: 50, GenStd: 0.5, PromptTPS: 2000, PromptStd: 10}
	for _, c := range []struct {
		r    Result
		want bool
	}{
		{Result{GenTPS: 51, PromptTPS: 2000}, false},               // inside 3%
		{Result{GenTPS: 52, PromptTPS: 2000}, true},                // over 3% and 3 sigma
		{Result{GenTPS: 50, PromptTPS: 2100}, true},                // gen level, prompt 5% better
		{Result{GenTPS: 47, PromptTPS: 3000}, false},               // gen worse
		{Result{GenTPS: 60, GenStd: 5, PromptTPS: 2000}, false},    // noisy
		{Result{GenTPS: 80, PromptTPS: 2000, Error: "oom"}, false}, // failed
	} {
		if got := better(c.r, base); got != c.want {
			t.Errorf("%+v: %v", c.r, got)
		}
	}
}
