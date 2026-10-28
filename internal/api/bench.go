package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/hostctl"
)

// BackendResult is one backend's llama-bench result.
type BackendResult struct {
	PromptTPS float64 `json:"prompt_tokens_per_second,omitempty"`
	GenTPS    float64 `json:"generation_tokens_per_second,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Benchmark is the state of a model's backend benchmark.
type Benchmark struct {
	Status      string                   `json:"status"` // queued, running, done or failed
	Updated     string                   `json:"updated,omitempty"`
	Results     map[string]BackendResult `json:"results,omitempty"`
	Recommended string                   `json:"recommended,omitempty"`
}

// benchBackends are the backend images that the host agent benchmarks: every
// backend that this variant ships.
func (s *Server) benchBackends() []string {
	var out []string
	for _, b := range hardware.Backends(s.Variant) {
		out = append(out, string(b))
	}
	return out
}

// runnableBackends are the variant's backends that can run on the
// selected GPUs: HIP only when the image supports every selected GPU.
func (s *Server) runnableBackends(hw hardware.Config) []string {
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	var out []string
	for _, b := range s.benchBackends() {
		if b == string(hardware.BackendHIP) && slices.ContainsFunc(hw.GPUs, func(i int) bool {
			return i >= 0 && i < len(gpus) && !hardware.HIPSupported(gpus[i].Arch)
		}) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func (s *Server) benchDir() string { return filepath.Join(s.Host.Dir, "bench") }

func (s *Server) startBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.findModel(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	hw, err := s.hardware()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	run := s.runnableBackends(hw)
	gpus := append([]int(nil), hw.GPUs...)
	sort.Ints(gpus)
	list := make([]string, len(gpus))
	for i, g := range gpus {
		list[i] = strconv.Itoa(g)
	}
	dir := s.benchDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, b := range s.benchBackends() {
		_ = os.Remove(filepath.Join(dir, id+"."+b+".json"))
		_ = os.Remove(filepath.Join(dir, id+"."+b+".log"))
	}
	files := map[string]string{
		".path": filepath.ToSlash(m.Path),
		".pool": m.Pool,
		".gpus": strings.Join(list, ","),
		// The agent runs only these backends. HIP is left out when a
		// selected GPU has a ROCm target that the HIP image lacks.
		".backends": strings.Join(run, " "),
		".status":   "queued " + time.Now().UTC().Format(time.RFC3339),
	}
	for ext, body := range files {
		if err := hardware.WriteFileAtomic(filepath.Join(dir, id+ext), []byte(body), 0o640); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.Host.Request(hostctl.Bench, id); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, Benchmark{Status: "queued"})
}

func (s *Server) getBenchmark(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBenchmark(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no benchmark for this model"))
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// readBenchmark reads the agent's status and the llama-bench output files.
func (s *Server) readBenchmark(id string) (Benchmark, bool) {
	dir := s.benchDir()
	raw, err := os.ReadFile(filepath.Join(dir, id+".status"))
	if err != nil {
		return Benchmark{}, false
	}
	status, updated, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	b := Benchmark{Status: status, Updated: updated}
	if status == "failed" {
		b.Status, b.Updated = "failed", ""
		return b, true
	}
	if status != "done" {
		return b, true
	}
	b.Results = map[string]BackendResult{}
	ran := s.benchBackends()
	if raw, err := os.ReadFile(filepath.Join(dir, id+".backends")); err == nil {
		ran = strings.Fields(string(raw))
	}
	for _, backend := range s.benchBackends() {
		if !slices.Contains(ran, backend) {
			b.Results[backend] = BackendResult{Error: "not run: the HIP image does not support a selected GPU"}
			continue
		}
		b.Results[backend] = parseBench(filepath.Join(dir, id+"."+backend+".json"), filepath.Join(dir, id+"."+backend+".log"))
	}
	// Token generation speed decides: it is what a user waits for.
	for _, backend := range s.benchBackends() {
		if r := b.Results[backend]; r.Error == "" && r.GenTPS > b.Results[b.Recommended].GenTPS {
			b.Recommended = backend
		}
	}
	return b, true
}

// parseBench reads llama-bench JSON output: one entry per test, with
// n_prompt and n_gen saying which test it was.
func parseBench(jsonPath, logPath string) BackendResult {
	var res BackendResult
	raw, err := os.ReadFile(jsonPath)
	var tests []struct {
		NPrompt int     `json:"n_prompt"`
		NGen    int     `json:"n_gen"`
		AvgTS   float64 `json:"avg_ts"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &tests)
	}
	if err != nil || len(tests) == 0 {
		res.Error = lastLine(logPath)
		if res.Error == "" {
			res.Error = "llama-bench produced no results"
		}
		return res
	}
	for _, t := range tests {
		switch {
		case t.NPrompt > 0 && t.NGen == 0:
			res.PromptTPS = t.AvgTS
		case t.NGen > 0 && t.NPrompt == 0:
			res.GenTPS = t.AvgTS
		}
	}
	return res
}

func lastLine(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return fmt.Sprintf("%.300s", l)
		}
	}
	return ""
}
