package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/hardware"
)

// HuggingFace is the Hugging Face sign-in state.
type HuggingFace struct {
	Configured bool   `json:"configured"`
	User       string `json:"user,omitempty"`
	SetAt      string `json:"set_at,omitempty"`
}

func (s *Server) hfTokenFile() string   { return filepath.Join(s.Paths.Config, "hf-token") }
func (s *Server) hfAccountFile() string { return filepath.Join(s.Paths.Config, "huggingface.json") }

// LoadHFToken returns the stored token, or an empty string.
func LoadHFToken(configDir string) string {
	b, err := os.ReadFile(filepath.Join(configDir, "hf-token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) getHuggingFace(w http.ResponseWriter, _ *http.Request) {
	var st HuggingFace
	if b, err := os.ReadFile(s.hfAccountFile()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	st.Configured = s.HF.HasToken()
	if !st.Configured {
		st = HuggingFace{}
	}
	writeJSON(w, http.StatusOK, st)
}

// putHuggingFace checks a token with the Hub, then stores it. Searches and
// downloads use it at once, so gated and private repositories work.
func (s *Server) putHuggingFace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") || len(token) > 512 {
		writeError(w, http.StatusUnprocessableEntity, errors.New("token: paste a Hugging Face access token, which starts with hf_"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	user, err := s.HF.WhoAmI(ctx, token)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := hardware.WriteFileAtomic(s.hfTokenFile(), []byte(token+"\n"), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st := HuggingFace{Configured: true, User: user, SetAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(st)
	if err := hardware.WriteFileAtomic(s.hfAccountFile(), b, 0o640); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.HF.SetToken(token)
	s.Log.Info("hugging face token set", "user", user)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) deleteHuggingFace(w http.ResponseWriter, _ *http.Request) {
	for _, f := range []string{s.hfTokenFile(), s.hfAccountFile()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.HF.SetToken("")
	writeJSON(w, http.StatusOK, HuggingFace{})
}
