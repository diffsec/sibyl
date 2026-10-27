package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

var benchBackends = []string{"hip", "vulkan"}

func (s *Server) benchDir() string { return filepath.Join(s.Host.Dir, "bench") }

func (s *Server) startBenchmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.findModel(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	hw, err := hardware.Load(s.Paths.hardwareFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
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
	for _, b := range benchBackends {
		_ = os.Remove(filepath.Join(dir, id+"."+b+".json"))
		_ = os.Remove(filepath.Join(dir, id+"."+b+".log"))
	}
	files := map[string]string{
		".path":   filepath.ToSlash(m.Path),
		".pool":   m.Pool,
		".gpus":   strings.Join(list, ","),
		".status": "queued " + time.Now().UTC().Format(time.RFC3339),
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
	for _, backend := range benchBackends {
		b.Results[backend] = parseBench(filepath.Join(dir, id+"."+backend+".json"), filepath.Join(dir, id+"."+backend+".log"))
	}
	// Token generation speed decides: it is what a user waits for.
	for _, backend := range benchBackends {
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
