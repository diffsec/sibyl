// Package api is the Sibyl System API. Caddy serves it under /system on port 8443.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/sibyl/internal/download"
	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/hf"
	"github.com/ihavespoons/sibyl/internal/hostctl"
	"github.com/ihavespoons/sibyl/internal/llamaswap"
	"github.com/ihavespoons/sibyl/internal/models"
	"github.com/ihavespoons/sibyl/internal/ollama"
)

// Paths are the directories that the API reads and writes, as seen inside its container.
type Paths struct {
	Config    string // hardware.yaml, models.yaml, llama-swap/config.yaml
	Models    string // GGUF files
	Pools     string // one directory per model pool, such as pool-1
	DropIn    string // llama-swap.container.d in the sibyl user's Quadlet directory
	SysfsRoot string // normally /sys
	MemInfo   string // normally /proc/meminfo
}

func (p Paths) hardwareFile() string  { return filepath.Join(p.Config, "hardware.yaml") }
func (p Paths) settingsFile() string  { return filepath.Join(p.Config, "models.yaml") }
func (p Paths) llamaSwapFile() string { return filepath.Join(p.Config, "llama-swap", "config.yaml") }
func (p Paths) dropInFile() string    { return filepath.Join(p.DropIn, "backend.conf") }

// Server holds the API state.
type Server struct {
	Paths     Paths
	Host      *hostctl.Client
	HostAgent *hostctl.HostClient // privileged host actions, nil if not configured
	HostWait  time.Duration       // 0 means 45 seconds
	Token     string
	Registry  string
	ImageTag  string
	// Variant is the host image's GPU variant. It decides the backends.
	Variant   hardware.Variant
	Version   string
	Log       *slog.Logger
	HF        *hf.Client
	Downloads *download.Manager
	Ollama    *ollama.Shim
	OpenWebUI *OpenWebUI
	// GitHubBase is https://github.com, or a test server.
	GitHubBase string

	// mu serializes every change to the configuration files.
	mu sync.Mutex
}

// Handler returns the HTTP routes. Every route except /health needs the bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	auth := http.NewServeMux()
	auth.HandleFunc("GET /status", s.getStatus)
	auth.HandleFunc("GET /gpus", s.getGPUs)
	auth.HandleFunc("GET /hardware", s.getHardware)
	auth.HandleFunc("PUT /hardware", s.putHardware)
	auth.HandleFunc("GET /models", s.getModels)
	auth.HandleFunc("PUT /models/{id}", s.putModel)
	auth.HandleFunc("DELETE /models/{id}", s.deleteModel)
	auth.HandleFunc("POST /models/{id}/benchmark", s.startBenchmark)
	auth.HandleFunc("GET /models/{id}/benchmark", s.getBenchmark)
	auth.HandleFunc("POST /apply", s.postApply)
	auth.HandleFunc("GET /services", s.getServices)
	auth.HandleFunc("GET /integrations/open-webui", s.getOpenWebUI)
	auth.HandleFunc("PUT /integrations/open-webui", s.putOpenWebUI)
	auth.HandleFunc("DELETE /integrations/open-webui", s.deleteOpenWebUI)
	auth.HandleFunc("POST /integrations/open-webui/connect", s.connectOpenWebUI)
	auth.HandleFunc("POST /services/{name}/restart", s.restartService)
	auth.HandleFunc("GET /host", s.getHost)
	auth.HandleFunc("GET /host/requests/{id}", s.getHostRequest)
	auth.HandleFunc("PUT /host/hostname", s.putHostname)
	auth.HandleFunc("PUT /host/network/{iface}", s.putNetwork)
	auth.HandleFunc("POST /host/network/confirm", s.confirmNetwork)
	auth.HandleFunc("PUT /host/users/{name}", s.putUser)
	auth.HandleFunc("DELETE /host/users/{name}", s.deleteUser)
	auth.HandleFunc("GET /pools", s.getPools)
	auth.HandleFunc("POST /pools", s.createPool)
	auth.HandleFunc("DELETE /pools/{name}", s.detachPool)
	auth.HandleFunc("GET /integrations/huggingface", s.getHuggingFace)
	auth.HandleFunc("PUT /integrations/huggingface", s.putHuggingFace)
	auth.HandleFunc("DELETE /integrations/huggingface", s.deleteHuggingFace)
	auth.HandleFunc("GET /hf/search", s.hfSearch)
	auth.HandleFunc("GET /hf/repos/{owner}/{name}", s.hfRepo)
	auth.HandleFunc("GET /downloads", s.listDownloads)
	auth.HandleFunc("POST /downloads", s.addDownload)
	auth.HandleFunc("GET /downloads/{id}", s.getDownload)
	auth.HandleFunc("DELETE /downloads/{id}", s.cancelDownload)
	auth.HandleFunc("POST /downloads/{id}/retry", s.retryDownload)
	if s.Ollama != nil {
		s.Ollama.Register(auth)
	}
	mux.Handle("/", s.requireToken(auth))
	return mux
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid API key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) getStatus(w http.ResponseWriter, _ *http.Request) {
	gpus, gerr := gpu.Detect(s.Paths.SysfsRoot)
	st, serr := s.Host.Status()
	hw, herr := hardware.Load(s.Paths.hardwareFile(), s.Variant)
	resp := map[string]any{
		"version":   s.Version,
		"image_tag": s.ImageTag,
		"variant":   s.Variant,
		"backends":  hardware.Backends(s.Variant),
		"gpus":      len(gpus),
		"hardware":  hw,
		"services":  st,
	}
	var errs []string
	for _, e := range []error{gerr, serr, herr} {
		if e != nil {
			errs = append(errs, e.Error())
		}
	}
	if len(errs) > 0 {
		resp["errors"] = errs
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getGPUs(w http.ResponseWriter, _ *http.Request) {
	gpus, err := gpu.Detect(s.Paths.SysfsRoot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, gpus)
}

func (s *Server) getHardware(w http.ResponseWriter, _ *http.Request) {
	hw, err := hardware.Load(s.Paths.hardwareFile(), s.Variant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, hw)
}

func (s *Server) putHardware(w http.ResponseWriter, r *http.Request) {
	var hw hardware.Config
	if err := readJSON(r, &hw); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	gpus, err := gpu.Detect(s.Paths.SysfsRoot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := hw.Validate(s.Variant, len(gpus)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := hardware.Save(s.Paths.hardwareFile(), hw); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.respondApply(w)
}

type modelView struct {
	models.Model
	Settings  models.Settings `json:"settings"`
	Benchmark *Benchmark      `json:"benchmark,omitempty"`
}

func (s *Server) getModels(w http.ResponseWriter, _ *http.Request) {
	installed, err := models.ScanAll(s.Paths.Models, s.Paths.Pools)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]modelView, len(installed))
	for i, m := range installed {
		out[i] = modelView{Model: m, Settings: settings[m.ID]}
		if b, ok := s.readBenchmark(m.ID); ok {
			out[i].Benchmark = &b
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var set models.Settings
	if err := readJSON(r, &set); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.findModel(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if d := set.Speculative.DraftModel; d != "" {
		if d == id {
			writeError(w, http.StatusUnprocessableEntity, errors.New("speculative.draft_model: a model cannot be its own draft model"))
			return
		}
		if _, err := s.findModel(d); err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("speculative.draft_model: %w", err))
			return
		}
	}
	hw, err := hardware.Load(s.Paths.hardwareFile(), s.Variant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := set.Validate(len(hw.GPUs)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	settings[id] = set
	if err := models.SaveSettings(s.Paths.settingsFile(), settings); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.respondApply(w)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.findModel(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := removeModelFiles(m.Root(s.Paths.Models, s.Paths.Pools), m); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err == nil {
		delete(settings, id)
		err = models.SaveSettings(s.Paths.settingsFile(), settings)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.respondApply(w)
}

func (s *Server) postApply(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respondApply(w)
}

func (s *Server) getServices(w http.ResponseWriter, _ *http.Request) {
	st, err := s.Host.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) restartService(w http.ResponseWriter, r *http.Request) {
	if err := s.Host.Request(hostctl.Restart, r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// ApplyResult reports what an apply changed.
type ApplyResult struct {
	Models         int               `json:"models"`
	Issues         []llamaswap.Issue `json:"issues,omitempty"`
	ConfigChanged  bool              `json:"config_changed"`
	BackendChanged bool              `json:"backend_changed"`
}

// Apply regenerates the llama-swap config and the backend drop-in. llama-swap
// reloads its config on its own. A changed drop-in needs a daemon-reload and a
// restart, which the host agent does. The caller must hold s.mu.
func (s *Server) Apply() (ApplyResult, error) {
	hw, err := hardware.Load(s.Paths.hardwareFile(), s.Variant)
	if err != nil {
		return ApplyResult{}, err
	}
	installed, err := models.ScanAll(s.Paths.Models, s.Paths.Pools)
	if err != nil {
		return ApplyResult{}, err
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return ApplyResult{}, err
	}

	cfg, issues := llamaswap.Generate(hw, installed, settings)
	body, err := llamaswap.Marshal(cfg)
	if err != nil {
		return ApplyResult{}, err
	}
	// llama-swap reloads on any change to the file's size or modification
	// time, and a reload unloads the running models. Write only on a change.
	res := ApplyResult{Models: len(cfg.Models), Issues: issues}
	if cur, err := os.ReadFile(s.Paths.llamaSwapFile()); err != nil || !bytes.Equal(cur, body) {
		if err := hardware.WriteFileAtomic(s.Paths.llamaSwapFile(), body, 0o640); err != nil {
			return ApplyResult{}, err
		}
		res.ConfigChanged = true
	}

	dropIn := []byte(llamaswap.DropIn(s.Variant, hw, s.Registry, s.ImageTag))
	old, err := os.ReadFile(s.Paths.dropInFile())
	if errors.Is(err, os.ErrNotExist) {
		// No override yet, so the image default is in effect. It matches the
		// drop-in for the default hardware (see TestImageDefaultDropIn), so a
		// first boot with no hardware.yaml does not restart llama-swap
		// in the middle of its first image pull.
		old, err = []byte(llamaswap.DropIn(s.Variant, hardware.Default(s.Variant), s.Registry, s.ImageTag)), nil
	}
	if err != nil {
		return res, err
	}
	if !bytes.Equal(old, dropIn) {
		if err := hardware.WriteFileAtomic(s.Paths.dropInFile(), dropIn, 0o644); err != nil {
			return res, err
		}
		res.BackendChanged = true
		if err := s.Host.Request(hostctl.Reload, ""); err != nil {
			return res, err
		}
		if err := s.Host.Request(hostctl.Restart, "llama-swap"); err != nil {
			return res, err
		}
	}
	for _, is := range issues {
		s.Log.Warn("model issue", "id", is.ID, "reason", is.Reason, "skipped", is.Skipped)
	}
	return res, nil
}

func (s *Server) respondApply(w http.ResponseWriter) {
	res, err := s.Apply()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("apply: %w", err))
		return
	}
	go s.SyncOpenWebUI()
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) findModel(id string) (models.Model, error) {
	installed, err := models.ScanAll(s.Paths.Models, s.Paths.Pools)
	if err != nil {
		return models.Model{}, err
	}
	for _, m := range installed {
		if m.ID == id {
			return m, nil
		}
	}
	return models.Model{}, fmt.Errorf("model %q is not installed", id)
}

// removeModelFiles deletes a model and all of its shards. Every path is
// checked to be inside the models directory first.
func removeModelFiles(dir string, m models.Model) error {
	first := filepath.Join(dir, m.Path)
	paths := []string{first}
	if m.Shards > 1 {
		// <base>-00001-of-0000N.gguf -> <base>-*-of-0000N.gguf
		i := strings.LastIndex(first, "-00001-of-")
		if i < 0 {
			return fmt.Errorf("model %q: cannot find its shards", m.ID)
		}
		matches, err := filepath.Glob(first[:i] + "-[0-9][0-9][0-9][0-9][0-9]" + first[i+len("-00001"):])
		if err != nil {
			return err
		}
		paths = matches
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return fmt.Errorf("refusing to delete %s: outside %s", abs, root)
		}
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

const maxBody = 1 << 20

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// OllamaModels lists installed models for the Ollama-compatible API.
func (s *Server) OllamaModels(context.Context) ([]ollama.ModelInfo, error) {
	installed, err := models.ScanAll(s.Paths.Models, s.Paths.Pools)
	if err != nil {
		return nil, err
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return nil, err
	}
	out := make([]ollama.ModelInfo, 0, len(installed))
	for _, m := range installed {
		st := settings[m.ID]
		if st.Hidden {
			continue
		}
		out = append(out, ollama.ModelInfo{ID: m.ID, Size: m.Size, Embedding: st.Embedding})
	}
	return out, nil
}
