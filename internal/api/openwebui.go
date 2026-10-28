package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
	"github.com/diffsec/sibyl/internal/owui"
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
	installed, err := models.ScanAll(s.Paths.Models, s.Paths.Pools)
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

type connectRequest struct {
	// CreateAdmin creates the first Open WebUI account, on a fresh install.
	CreateAdmin bool   `json:"create_admin"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

// connectOpenWebUI logs in to Open WebUI as an admin (or creates the first
// admin), turns on API keys, gets the admin's API key, and stores only the
// key. The password is used for this request only.
func (s *Server) connectOpenWebUI(w http.ResponseWriter, r *http.Request) {
	if s.OpenWebUI == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the Open WebUI integration is not configured"))
		return
	}
	var req connectRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" || len(req.Password) > 256 {
		writeError(w, http.StatusBadRequest, errors.New("give the admin email and password"))
		return
	}
	if req.CreateAdmin && strings.TrimSpace(req.Name) == "" {
		req.Name = "Admin"
	}

	c := owui.New(s.OpenWebUI.Base, "")
	var token string
	var err error
	if req.CreateAdmin {
		token, err = c.SignUp(r.Context(), req.Name, req.Email, req.Password)
	} else {
		token, err = c.SignIn(r.Context(), req.Email, req.Password)
	}
	req.Password = ""
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if _, err := c.EnableAPIKeys(r.Context(), token); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("turn on API keys: %w", err))
		return
	}
	key, err := c.APIKey(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("get API key: %w", err))
		return
	}
	if err := hardware.WriteFileAtomic(s.owuiKeyFile(), []byte(key), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.SyncOpenWebUI()
	s.getOpenWebUI(w, r)
}
