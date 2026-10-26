package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/models"
	"github.com/ihavespoons/sibyl/internal/owui"
)

// OpenWebUI holds the Open WebUI integration state.
type OpenWebUI struct {
	Base     string // http://open-webui:8080
	EmbedURL string // OpenAI base URL that Open WebUI uses for embeddings
	// syncMu runs one sync at a time. A later sync waits rather than
	// skipping, so the last configuration change always reaches Open WebUI.
	syncMu     sync.Mutex
	mu         sync.Mutex
	lastSync   string
	lastErr    string
	lastSyncAt time.Time
}

func (s *Server) owuiKeyFile() string { return filepath.Join(s.Paths.Config, "open-webui.key") }

func (s *Server) owuiClient() (*owui.Client, error) {
	key, err := os.ReadFile(s.owuiKeyFile())
	if err != nil {
		return nil, err
	}
	return owui.New(s.OpenWebUI.Base, strings.TrimSpace(string(key))), nil
}

// embeddingModel is the first installed model marked as an embedding model.
func (s *Server) embeddingModel() (string, error) {
	installed, err := models.Scan(s.Paths.Models)
	if err != nil {
		return "", err
	}
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return "", err
	}
	var ids []string
	for _, m := range installed {
		if settings[m.ID].Embedding {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// SyncOpenWebUI points Open WebUI at the embedding model, if a key is stored.
// It runs in the background after each configuration change.
func (s *Server) SyncOpenWebUI() {
	if s.OpenWebUI == nil {
		return
	}
	o := s.OpenWebUI
	o.syncMu.Lock()
	defer o.syncMu.Unlock()

	result, err := s.syncOnce()
	o.mu.Lock()
	o.lastSync, o.lastSyncAt = result, time.Now().UTC()
	o.lastErr = ""
	if err != nil {
		o.lastErr = err.Error()
		s.Log.Warn("open webui sync", "err", err)
	}
	o.mu.Unlock()
}

func (s *Server) syncOnce() (string, error) {
	c, err := s.owuiClient()
	if errors.Is(err, os.ErrNotExist) {
		return "no key stored", nil
	}
	if err != nil {
		return "", err
	}
	model, err := s.embeddingModel()
	if err != nil {
		return "", err
	}
	if model == "" {
		return "no embedding model installed", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	changed, err := c.Sync(ctx, model, s.OpenWebUI.EmbedURL)
	if err != nil {
		return "", err
	}
	if changed {
		s.Log.Info("open webui embedding model set", "model", model)
		return "set embedding model " + model, nil
	}
	return "embedding model " + model + " already set", nil
}

type owuiStatus struct {
	Configured bool   `json:"configured"`
	Desired    string `json:"embedding_model,omitempty"`
	LastSync   string `json:"last_sync,omitempty"`
	LastSyncAt string `json:"last_sync_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) getOpenWebUI(w http.ResponseWriter, _ *http.Request) {
	if s.OpenWebUI == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the Open WebUI integration is not configured"))
		return
	}
	_, err := os.Stat(s.owuiKeyFile())
	st := owuiStatus{Configured: err == nil}
	st.Desired, _ = s.embeddingModel()
	o := s.OpenWebUI
	o.mu.Lock()
	st.LastSync, st.Error = o.lastSync, o.lastErr
	if !o.lastSyncAt.IsZero() {
		st.LastSyncAt = o.lastSyncAt.Format(time.RFC3339)
	}
	o.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putOpenWebUI(w http.ResponseWriter, r *http.Request) {
	if s.OpenWebUI == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the Open WebUI integration is not configured"))
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		writeError(w, http.StatusBadRequest, errors.New("api_key: give the Open WebUI API key"))
		return
	}
	// Check the key before storing it: it must reach an admin-only endpoint.
	if _, err := owui.New(s.OpenWebUI.Base, key).Embedding(r.Context()); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := hardware.WriteFileAtomic(s.owuiKeyFile(), []byte(key), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.SyncOpenWebUI()
	s.getOpenWebUI(w, r)
}

func (s *Server) deleteOpenWebUI(w http.ResponseWriter, r *http.Request) {
	if err := os.Remove(s.owuiKeyFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.getOpenWebUI(w, r)
}
