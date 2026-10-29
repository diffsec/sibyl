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

	"github.com/diffsec/sibyl/internal/download"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/hf"
	"github.com/diffsec/sibyl/internal/hostctl"
	"github.com/diffsec/sibyl/internal/llamaswap"
	"github.com/diffsec/sibyl/internal/models"
	"github.com/diffsec/sibyl/internal/ollama"
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
	Arcane    *Arcane
	Catalog   *Catalog
	// GitHubBase is https://github.com, or a test server.
	GitHubBase string
	// RouterURL is the model router, such as http://router:8080.
	RouterURL string
	// ProviderHTTP checks provider keys. Nil means a default client.
	ProviderHTTP *http.Client

	// mu serializes every change to the configuration files.
	mu sync.Mutex

	metaMu    sync.Mutex
	metaCache map[string]cachedMeta
}

// cachedMeta is a model's GGUF metadata, valid while the file keeps its
// size and modification time.
type cachedMeta struct {
	size int64
	mod  time.Time
	meta *models.Meta
}

// modelMeta reads the GGUF metadata of a model's first file, or returns nil
// when the header cannot be read.
func (s *Server) modelMeta(m models.Model) *models.Meta {
	path := filepath.Join(m.Root(s.Paths.Models, s.Paths.Pools), m.Path)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if c, ok := s.metaCache[path]; ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.meta
	}
	var meta *models.Meta
	if mt, err := models.ReadMeta(path); err == nil {
		meta = &mt
	} else {
		s.Log.Warn("read model metadata", "model", m.ID, "err", err)
	}
	if s.metaCache == nil {
		s.metaCache = map[string]cachedMeta{}
	}
	s.metaCache[path] = cachedMeta{size: fi.Size(), mod: fi.ModTime(), meta: meta}
	return meta
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
	auth.HandleFunc("GET /integrations/arcane", s.getArcane)
	auth.HandleFunc("GET /router", s.getRouter)
	auth.HandleFunc("POST /router/clients", s.postRouterClient)
	auth.HandleFunc("PUT /router/clients/{name}", s.putRouterClient)
	auth.HandleFunc("POST /router/clients/{name}/key", s.postRouterClientKey)
	auth.HandleFunc("DELETE /router/clients/{name}", s.deleteRouterClient)
	auth.HandleFunc("PUT /router/providers/{name}", s.putRouterProvider)
	auth.HandleFunc("DELETE /router/providers/{name}", s.deleteRouterProvider)
	auth.HandleFunc("PUT /router/profiles", s.putRouterProfiles)
	auth.HandleFunc("POST /router/explain", s.postRouterExplain)
	auth.HandleFunc("GET /router/stats", s.getRouterStats)
	auth.HandleFunc("GET /router/catalog", s.getCatalog)
	auth.HandleFunc("GET /router/catalog/{id}", s.getCatalogProvider)
	auth.HandleFunc("POST /router/catalog/refresh", s.postCatalogRefresh)
	// The Ollama shim is for inference clients, so it takes router client
	// keys, not the System API key. The router checks them.
	if s.Ollama != nil {
		s.Ollama.Register(mux)
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
	hw, herr := hardware.Load(s.Paths.hardwareFile(), s.Variant, gpus)
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
	hw, err := s.hardware()
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
	if err := hw.Validate(s.Variant, gpus); err != nil {
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
	Settings models.Settings `json:"settings"`
	// Fit is the context size and KV cache types that the model runs
	// with, which Sibyl chooses where the settings leave them open.
	Fit       *models.Fit `json:"fit,omitempty"`
	Benchmark *Benchmark  `json:"benchmark,omitempty"`
	// Meta is what the model file says about its context, for the context
	// size and memory estimate. It is missing when the header is unreadable.
	Meta *models.Meta `json:"meta,omitempty"`
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
	hw, _ := s.hardware()
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	out := make([]modelView, len(installed))
	for i, m := range installed {
		_, fit := s.effective(m, settings[m.ID], hw, gpus)
		out[i] = modelView{Model: m, Settings: settings[m.ID], Meta: s.modelMeta(m), Fit: fit}
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
	hw, err := s.hardware()
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
	hw, err := s.hardware()
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

	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	eff := make(map[string]models.Settings, len(installed))
	for _, m := range installed {
		eff[m.ID], _ = s.effective(m, settings[m.ID], hw, gpus)
	}
	cfg, issues := llamaswap.Generate(hw, installed, eff)
	body, err := llamaswap.Marshal(cfg)
	if err != nil {
		return ApplyResult{}, err
	}
	served := map[string]bool{}
	for id := range cfg.Models {
		served[id] = true
	}
	// The router reads each model's context size from models.json. A
	// failure here does not stop llama-swap from getting its config.
	if err := s.writeRouterModels(installed, eff, served); err != nil {
		s.Log.Warn("write the router's model list", "err", err)
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
		// drop-in for the default hardware with no GPU known (see
		// TestImageDefaultDropIn), so a first boot with no hardware.yaml does
		// not restart llama-swap in the middle of its first image pull. On a
		// GPU that the HIP image does not support, the default is Vulkan, and
		// the drop-in changes once.
		old, err = []byte(llamaswap.DropIn(s.Variant, hardware.Default(s.Variant, nil), s.Registry, s.ImageTag)), nil
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

// effective returns the settings that a model runs with: the automatic
// context fit and the preset's sampling, under the owner's own values. The
// result is not saved: models.yaml keeps only what the owner chose.
func (s *Server) effective(m models.Model, st models.Settings, hw hardware.Config, gpus []gpu.GPU) (models.Settings, *models.Fit) {
	meta := s.modelMeta(m)
	st.Sampling = st.EffectiveSampling(meta)
	if st.Embedding || meta == nil {
		return st, nil
	}
	vram := hardware.VRAM(hardware.ModelGPUs(st.Layout, st.Embedding, hw, gpus))
	fit := meta.FitContext(st.Fit, meta.Weights(m.Size), vram, st.CtxSize, st.KVCache.TypeK, st.KVCache.TypeV)
	if fit.Auto {
		st.CtxSize, st.KVCache.TypeK, st.KVCache.TypeV = fit.Ctx, fit.TypeK, fit.TypeV
	}
	return st, &fit
}

// hardware loads hardware.yaml. With no file, the default depends on the
// detected GPUs, so an AMD GPU that the HIP image does not support gets Vulkan.
func (s *Server) hardware() (hardware.Config, error) {
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	return hardware.Load(s.Paths.hardwareFile(), s.Variant, gpus)
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
