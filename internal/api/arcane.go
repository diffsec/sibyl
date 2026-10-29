package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/arcane"
	"github.com/diffsec/sibyl/internal/hardware"
)

// Arcane replaces the default admin password of a new Arcane install, so
// the LAN never meets arcane / arcane-admin for longer than Arcane takes to
// start. The new password stays in the configuration directory until the
// owner changes it in Arcane.
type Arcane struct {
	Client *arcane.Client
	mu     sync.Mutex
	err    string
	// checked is when getArcane last signed in with the stored password.
	// It signs in at most once a minute, because the TUI polls.
	checked time.Time
}

// recheck is how often getArcane checks the stored password with Arcane.
const recheck = time.Minute

func (s *Server) arcanePasswordFile() string { return filepath.Join(s.Paths.Config, "arcane-password") }

func newPassword() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(b)), nil
}

// secureArcane replaces the default password if Arcane still accepts it.
func (s *Server) secureArcane(ctx context.Context) error {
	c := s.Arcane.Client
	token, err := c.Login(ctx, arcane.DefaultUser, arcane.DefaultPassword)
	if errors.Is(err, arcane.ErrLogin) {
		return nil // already changed
	}
	if err != nil {
		return err
	}
	pw, err := newPassword()
	if err != nil {
		return err
	}
	// Store the password first: a crash after the change must not lose it.
	if err := hardware.WriteFileAtomic(s.arcanePasswordFile(), []byte(pw+"\n"), 0o600); err != nil {
		return err
	}
	if err := c.ChangePassword(ctx, token, arcane.DefaultPassword, pw); err != nil {
		_ = os.Remove(s.arcanePasswordFile())
		return err
	}
	s.Log.Info("arcane default password replaced")
	return nil
}

// SecureArcaneAtStart replaces Arcane's default password once Arcane
// answers. It tries every interval, up to tries times.
func (s *Server) SecureArcaneAtStart(ctx context.Context, interval time.Duration, tries int) {
	if s.Arcane == nil {
		return
	}
	for i := 0; i < tries; i++ {
		err := s.secureArcane(ctx)
		s.Arcane.mu.Lock()
		s.Arcane.err = ""
		if err != nil {
			s.Arcane.err = err.Error()
		}
		s.Arcane.mu.Unlock()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
	s.Log.Warn("arcane default password not checked", "err", s.Arcane.err)
}

type arcaneStatus struct {
	User string `json:"user"`
	// Password is the password that Sibyl set, until the owner changes it.
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// getArcane returns the password that Sibyl set, while Arcane still
// accepts it. Once the owner changes it in Arcane, the file is deleted.
func (s *Server) getArcane(w http.ResponseWriter, r *http.Request) {
	st := arcaneStatus{User: arcane.DefaultUser}
	if s.Arcane != nil {
		s.Arcane.mu.Lock()
		st.Error = s.Arcane.err
		s.Arcane.mu.Unlock()
	}
	b, err := os.ReadFile(s.arcanePasswordFile())
	if err != nil || s.Arcane == nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	pw := strings.TrimSpace(string(b))
	s.Arcane.mu.Lock()
	fresh := time.Since(s.Arcane.checked) < recheck
	if !fresh {
		s.Arcane.checked = time.Now()
	}
	s.Arcane.mu.Unlock()
	if fresh {
		st.Password = pw
		writeJSON(w, http.StatusOK, st)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, err = s.Arcane.Client.Login(ctx, arcane.DefaultUser, pw)
	switch {
	case errors.Is(err, arcane.ErrLogin):
		_ = os.Remove(s.arcanePasswordFile())
	case err != nil:
		st.Error = err.Error()
		st.Password = pw
	default:
		st.Password = pw
	}
	writeJSON(w, http.StatusOK, st)
}
