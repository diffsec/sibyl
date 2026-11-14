package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

// loadTimeout bounds a model load. A 40 GB model loads from disk in about
// a minute, and llama-swap's health check allows 10.
const loadTimeout = 10 * time.Minute

// RunningModel is one model that llama-swap has started.
type RunningModel struct {
	ID string `json:"id"`
	// State is llama-swap's process state, such as "starting" or "ready".
	State string `json:"state"`
}

// Running is the state of the models that llama-swap runs.
type Running struct {
	Models []RunningModel `json:"models"`
	// Loading lists the models that a POST /models/{id}/load still waits
	// for, and Errors the last failed load of each model.
	Loading []string          `json:"loading,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// loads tracks the loads that the owner started.
type loads struct {
	mu      sync.Mutex
	pending map[string]bool
	errors  map[string]string
}

func (l *loads) start(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil {
		l.pending, l.errors = map[string]bool{}, map[string]string{}
	}
	if l.pending[id] {
		return false
	}
	l.pending[id] = true
	delete(l.errors, id)
	return true
}

func (l *loads) finish(id string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, id)
	if err != nil {
		l.errors[id] = err.Error()
	}
}

func (l *loads) view(r *Running) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.pending {
		r.Loading = append(r.Loading, id)
	}
	slices.Sort(r.Loading)
	if len(l.errors) > 0 {
		r.Errors = make(map[string]string, len(l.errors))
		for id, e := range l.errors {
			r.Errors[id] = e
		}
	}
}

// llamaSwapDo sends one request to llama-swap and returns its body.
func (s *Server) llamaSwapDo(ctx context.Context, method, path string) ([]byte, error) {
	if s.LlamaSwapURL == "" {
		return nil, errors.New("no llama-swap address")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.LlamaSwapURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("llama-swap %s: %s", path, resp.Status)
	}
	return body, nil
}

func (s *Server) getRunning(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	body, err := s.llamaSwapDo(ctx, http.MethodGet, "/running")
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	var raw struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("llama-swap /running: %w", err))
		return
	}
	out := Running{Models: []RunningModel{}}
	for _, m := range raw.Running {
		out.Models = append(out.Models, RunningModel{ID: m.Model, State: m.State})
	}
	slices.SortFunc(out.Models, func(a, b RunningModel) int { return cmp.Compare(a.ID, b.ID) })
	s.loads.view(&out)
	writeJSON(w, http.StatusOK, out)
}

// servedModel reports whether llama-swap can run model id on its own. A
// missing model or a drafter gets an error response.
func (s *Server) servedModel(w http.ResponseWriter, id string) bool {
	installed, err := s.scanModels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	i := slices.IndexFunc(installed, func(m models.Model) bool { return m.ID == id })
	switch {
	case i < 0:
		writeError(w, http.StatusNotFound, errString("model is not installed"))
		return false
	case installed[i].Drafter:
		writeError(w, http.StatusUnprocessableEntity, errString("a drafter loads with the model that uses it"))
		return false
	}
	return true
}

// loadModel asks llama-swap to start a model, through its upstream health
// route. llama-swap first stops the models that cannot run with it, as the
// matrix in its configuration says. The load runs in the background.
func (s *Server) loadModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.servedModel(w, id) {
		return
	}
	if err := s.ramShort(r.Context(), id); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if !s.loads.start(id) {
		writeError(w, http.StatusConflict, errString("the model is already loading"))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		_, err := s.llamaSwapDo(ctx, http.MethodGet, "/upstream/"+url.PathEscape(id)+"/health")
		if err != nil {
			s.Log.Warn("load model", "model", id, "err", err)
		}
		s.loads.finish(id, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "loading"})
}

// ramShort returns an error when the host does not have the free system
// RAM that model id keeps while it runs. Only an Edge0 model has such a
// need. A model that llama-swap already runs holds its RAM, so it passes.
// The launcher in the image makes the same check, for a load that a chat
// request starts.
func (s *Server) ramShort(ctx context.Context, id string) error {
	m, err := s.findModel(id)
	if err != nil || m.RAM == 0 {
		return nil
	}
	need := m.Edge0RAMOn(s.modelGPUs(id))
	avail := s.memInfo("MemAvailable:")
	if avail == 0 || avail >= need {
		return nil // enough, or the host does not report it
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if body, err := s.llamaSwapDo(ctx, http.MethodGet, "/running"); err == nil {
		var raw struct {
			Running []struct {
				Model string `json:"model"`
			} `json:"running"`
		}
		if json.Unmarshal(body, &raw) == nil {
			for _, r := range raw.Running {
				if r.Model == id {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("the model keeps %.1f GiB of system RAM, and %.1f GiB is available",
		float64(need)/(1<<30), float64(avail)/(1<<30))
}

// modelGPUs returns the number of GPUs that model id runs on: every
// selected GPU for a model that spans them, else 1. It answers 1 when the
// hardware or the settings cannot be read, which is the larger RAM need.
func (s *Server) modelGPUs(id string) int {
	hw, err := s.hardware()
	if err != nil {
		return 1
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return 1
	}
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	return max(1, len(hardware.ModelGPUs(settings[id].Layout, false, hw, gpus)))
}

func (s *Server) unloadModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.servedModel(w, id) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if _, err := s.llamaSwapDo(ctx, http.MethodPost, "/api/models/unload/"+url.PathEscape(id)); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded"})
}
