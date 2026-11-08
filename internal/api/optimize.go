package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/hostctl"
	"github.com/diffsec/sibyl/internal/models"
	"github.com/diffsec/sibyl/internal/optimize"
	"github.com/diffsec/sibyl/internal/router"
)

// OptimizeRequest starts an optimizer run.
type OptimizeRequest struct {
	Profile optimize.Profile `json:"profile"`
	// Advisor is an optional router provider model that proposes more
	// settings to measure.
	Advisor *OptimizeAdvisor `json:"advisor,omitempty"`
}

// OptimizeAdvisor names a provider model of the router.
type OptimizeAdvisor struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// OptimizeState is the state of a model's last optimizer run.
type OptimizeState struct {
	Status         string                   `json:"status"` // running, done, failed or cancelled
	Profile        optimize.Profile         `json:"profile"`
	Advisor        string                   `json:"advisor,omitempty"` // provider/model
	Stage          string                   `json:"stage,omitempty"`
	Started        string                   `json:"started"`
	Updated        string                   `json:"updated"`
	Results        []optimize.Result        `json:"results,omitempty"`
	Recommendation *optimize.Recommendation `json:"recommendation,omitempty"`
	Error          string                   `json:"error,omitempty"`
	// ImageTag is the backend image version of the run. A recommendation
	// is for that llama.cpp build.
	ImageTag string `json:"image_tag,omitempty"`
	// Applied is set when the owner applied the recommendation.
	Applied string `json:"applied,omitempty"`
}

// OptimizeApply applies a recommendation. Backend also switches the
// backend of every model, when the run found a faster one.
type OptimizeApply struct {
	Backend bool `json:"backend"`
}

// optimizeRuns allows one run at a time: a run unloads the models and uses
// every selected GPU.
type optimizeRuns struct {
	mu      sync.Mutex
	running string
	cancel  context.CancelFunc
}

func (s *Server) optimizeDir() string { return filepath.Join(s.Host.Dir, "optimize") }

func (s *Server) tuneDir() string { return filepath.Join(s.Host.Dir, "tune") }

func (s *Server) readOptimize(id string) (OptimizeState, bool) {
	raw, err := os.ReadFile(filepath.Join(s.optimizeDir(), id+".json"))
	if err != nil {
		return OptimizeState{}, false
	}
	var st OptimizeState
	if json.Unmarshal(raw, &st) != nil {
		return OptimizeState{}, false
	}
	return st, true
}

func (s *Server) writeOptimize(id string, st OptimizeState) error {
	st.Updated = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.optimizeDir(), 0o750); err != nil {
		return err
	}
	return hardware.WriteFileAtomic(filepath.Join(s.optimizeDir(), id+".json"), b, 0o640)
}

func (s *Server) startOptimize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req OptimizeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Profile == "" {
		req.Profile = optimize.Single
	}
	if req.Profile != optimize.Single && req.Profile != optimize.Throughput {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("profile %q: use %s or %s", req.Profile, optimize.Single, optimize.Throughput))
		return
	}
	in, runner, err := s.optimizeInput(id, req.Profile, req.Advisor)
	if err != nil {
		code := http.StatusUnprocessableEntity
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		writeError(w, code, err)
		return
	}
	s.optimize.mu.Lock()
	if s.optimize.running != "" {
		running := s.optimize.running
		s.optimize.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("the optimizer is running for %s", running))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.optimize.running, s.optimize.cancel = id, cancel
	s.optimize.mu.Unlock()

	st := OptimizeState{Status: "running", Profile: req.Profile, Stage: "baseline",
		Started: time.Now().UTC().Format(time.RFC3339), ImageTag: s.ImageTag}
	if req.Advisor != nil {
		st.Advisor = req.Advisor.Provider + "/" + req.Advisor.Model
	}
	if err := s.writeOptimize(id, st); err != nil {
		s.endOptimize()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	go s.runOptimize(ctx, id, st, in, runner)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) endOptimize() {
	s.optimize.mu.Lock()
	defer s.optimize.mu.Unlock()
	if s.optimize.cancel != nil {
		s.optimize.cancel()
	}
	s.optimize.running, s.optimize.cancel = "", nil
}

func (s *Server) runOptimize(ctx context.Context, id string, st OptimizeState, in optimize.Input, runner optimize.Runner) {
	defer s.endOptimize()
	var mu sync.Mutex
	search := &optimize.Search{Runner: runner, Progress: func(stage string, results []optimize.Result) {
		mu.Lock()
		defer mu.Unlock()
		st.Stage, st.Results = stage, results
		_ = s.writeOptimize(id, st)
	}}
	rec, err := search.Run(ctx, in)
	mu.Lock()
	defer mu.Unlock()
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		st.Status, st.Error = "cancelled", "the owner cancelled the run"
	case err != nil:
		st.Status, st.Error = "failed", err.Error()
	default:
		st.Status, st.Stage, st.Recommendation = "done", "done", rec
	}
	if err := s.writeOptimize(id, st); err != nil {
		s.Log.Error("optimizer state", "model", id, "err", err)
	}
}

// optimizeInput describes the model and the host for a run.
func (s *Server) optimizeInput(id string, profile optimize.Profile, adv *OptimizeAdvisor) (optimize.Input, optimize.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.findModel(id)
	if err != nil {
		return optimize.Input{}, nil, fmt.Errorf("%w: %v", os.ErrNotExist, err)
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return optimize.Input{}, nil, err
	}
	set := settings[id]
	if set.Embedding || m.Drafter {
		return optimize.Input{}, nil, errors.New("the optimizer measures chat models, not embedding models or drafters")
	}
	hw, err := s.hardware()
	if err != nil {
		return optimize.Input{}, nil, err
	}
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	installed, err := s.scanModels()
	if err != nil {
		return optimize.Input{}, nil, err
	}
	_, fits := s.effectiveAll(installed, settings, hw, gpus)
	used := hardware.ModelGPUs(set.Layout, false, hw, gpus)
	if len(used) == 0 {
		return optimize.Input{}, nil, errors.New("no selected GPU can run the model")
	}
	list := make([]string, len(used))
	for i, g := range used {
		list[i] = strconv.Itoa(g.Index)
	}
	in := optimize.Input{
		Profile: profile, Settings: set, Backend: string(hw.Backend), Backends: s.runnableBackends(hw), GPUs: len(used),
	}
	meta := s.modelMeta(m)
	fit := fits[id]
	if fit != nil {
		in.Ctx = fit.Ctx
	}
	if meta != nil {
		in.Layers = len(meta.ExpertBytes)
		// A KV cache type may not take more memory than the one that the
		// model runs with now, so the context still fits. The verify
		// stage checks the load at the real context.
		if fit != nil {
			now := meta.ContextBytes(fit.Ctx, fit.TypeK, fit.TypeV)
			in.KVFits = func(k, v string) bool { return meta.ContextBytes(fit.Ctx, k, v) <= now }
		}
		in.MoE = moeCandidates(set.MoE.CPULayers, in.Layers, meta.ExpertCount)
	}
	if adv != nil {
		rc, err := s.loadRouter()
		if err != nil {
			return optimize.Input{}, nil, err
		}
		if _, ok := rc.Providers.Providers[adv.Provider]; !ok || adv.Model == "" || strings.ContainsAny(adv.Model, " \t\r\n") {
			return optimize.Input{}, nil, fmt.Errorf("advisor: the router has no provider %q with a model %q", adv.Provider, adv.Model)
		}
		in.Advisor = optimize.LLMAdvisor{Ask: s.askAdvisor(adv.Provider, adv.Model), Facts: s.optimizeFacts(m, meta, used)}
	}
	runner := &agentRunner{s: s, id: id, rel: filepath.ToSlash(m.Path), pool: m.Pool, gpus: strings.Join(list, ","), poll: s.tunePoll}
	return in, runner, nil
}

// optimizeFacts describes the hardware and the model for the advisor. It
// holds no prompts, no user data and no file paths.
func (s *Server) optimizeFacts(m models.Model, meta *models.Meta, used []gpu.GPU) string {
	var b strings.Builder
	for i, g := range used {
		parts := []string{g.Vendor}
		if g.DeviceID != "" {
			parts = append(parts, "device "+g.DeviceID)
		}
		if g.Driver != "" {
			parts = append(parts, "driver "+g.Driver)
		}
		if g.VRAMTotal > 0 {
			parts = append(parts, fmt.Sprintf("%.1f GiB VRAM", float64(g.VRAMTotal)/(1<<30)))
		}
		if g.Arch != "" {
			parts = append(parts, "ROCm target "+g.Arch)
		}
		if g.LinkWidth != "" {
			parts = append(parts, fmt.Sprintf("PCIe %s at %s", g.LinkWidth, g.LinkSpeed))
		}
		if g.VisibleVRAM > 0 && g.VisibleVRAM < g.VRAMTotal {
			parts = append(parts, "Resizable BAR off")
		}
		fmt.Fprintf(&b, "GPU %d: %s\n", i, strings.Join(parts, ", "))
	}
	if _, ram, _ := s.memory(); ram > 0 {
		fmt.Fprintf(&b, "System RAM: %.1f GiB\n", float64(ram)/(1<<30))
	}
	fmt.Fprintf(&b, "Model file: %s, %.1f GiB\n", filepath.Base(m.Path), float64(m.Size)/(1<<30))
	if meta != nil {
		fmt.Fprintf(&b, "Architecture: %s, native context %d tokens, %d attention layers", meta.Architecture, meta.ContextLength, meta.AttentionLayers)
		if meta.ExpertCount > 0 {
			fmt.Fprintf(&b, ", %d experts per MoE layer", meta.ExpertCount)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// askAdvisor asks the router's /sibyl/advise route. The provider keys stay
// in the router.
func (s *Server) askAdvisor(provider, model string) func(ctx context.Context, system, prompt string) (string, error) {
	return func(ctx context.Context, system, prompt string) (string, error) {
		body, err := json.Marshal(router.AdviseRequest{Provider: provider, Model: model, System: system, Prompt: prompt})
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RouterURL+"/sibyl/advise", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+s.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("reach the router: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("the router answered %d: %.300s", resp.StatusCode, raw)
		}
		var ar router.AdviseResponse
		if err := json.Unmarshal(raw, &ar); err != nil {
			return "", err
		}
		return ar.Answer, nil
	}
}

// moeCandidates are the CPUMoE values to try: fewer layers with experts in
// system RAM than now. Fewer is faster, if it loads.
func moeCandidates(now, layers, experts int) []int {
	if experts == 0 || now == 0 {
		return nil
	}
	if now == models.AllLayers {
		now = layers
	}
	var out []int
	for _, d := range []int{2, 4, 8} {
		if n := now - d; n >= 0 {
			out = append(out, n)
		}
	}
	return out
}

// agentRunner runs optimizer tools through the host agent's tune request.
type agentRunner struct {
	s                   *Server
	id, rel, pool, gpus string
	// poll is the time between status reads. Tests shorten it.
	poll time.Duration
}

// tuneTimeout is the longest one tool run may take. The host agent's
// systemd unit stops a run after 45 minutes.
const tuneTimeout = 45 * time.Minute

func (a *agentRunner) Run(ctx context.Context, r optimize.Request) ([]byte, error) {
	dir := a.s.tuneDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	files := map[string]string{
		".path": a.rel, ".pool": a.pool, ".gpus": a.gpus, ".backend": r.Backend, ".tool": r.Tool,
		".args":   strings.Join(r.Args, "\n") + "\n",
		".status": "queued " + time.Now().UTC().Format(time.RFC3339),
	}
	for ext, body := range files {
		if err := hardware.WriteFileAtomic(filepath.Join(dir, a.id+ext), []byte(body), 0o640); err != nil {
			return nil, err
		}
	}
	if err := a.s.Host.Request(hostctl.Tune, a.id); err != nil {
		return nil, err
	}
	poll := a.poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(tuneTimeout)
	for {
		raw, _ := os.ReadFile(filepath.Join(dir, a.id+".status"))
		status, rest, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
		switch status {
		case "done":
			return os.ReadFile(filepath.Join(dir, a.id+".out"))
		case "failed:":
			return nil, fmt.Errorf("%s %s: %s", r.Tool, r.Backend, rest)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s %s: no result after %s", r.Tool, r.Backend, tuneTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (s *Server) getOptimize(w http.ResponseWriter, r *http.Request) {
	st, ok := s.readOptimize(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no optimizer run for this model"))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) cancelOptimize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.optimize.mu.Lock()
	running, cancel := s.optimize.running, s.optimize.cancel
	s.optimize.mu.Unlock()
	if running != id || cancel == nil {
		writeError(w, http.StatusNotFound, errors.New("no optimizer run is going for this model"))
		return
	}
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *Server) applyOptimize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req OptimizeApply
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, ok := s.readOptimize(id)
	if !ok || st.Status != "done" || st.Recommendation == nil {
		writeError(w, http.StatusNotFound, errors.New("no finished optimizer run for this model"))
		return
	}
	rec := st.Recommendation
	if !rec.Verified {
		writeError(w, http.StatusConflict, errors.New("the recommendation did not load at the model's context, so it cannot be applied"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if code, err := s.setModel(id, rec.Settings); err != nil {
		writeError(w, code, err)
		return
	}
	if req.Backend && rec.Backend != "" {
		hw, err := s.hardware()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		gpus, err := gpu.Detect(s.Paths.SysfsRoot)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		hw.Backend = hardware.Backend(rec.Backend)
		if err := hw.Validate(s.Variant, gpus); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		if err := hardware.Save(s.Paths.hardwareFile(), hw); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	st.Applied = time.Now().UTC().Format(time.RFC3339)
	_ = s.writeOptimize(id, st)
	s.respondApply(w)
}
