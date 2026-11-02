package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/diffsec/sibyl/internal/diag"
	"github.com/diffsec/sibyl/internal/hostd"
)

// The diagnostics routes read the summary files that sibyl-diag writes as
// root, and pass every change to sibyl-hostd. The System API never reads an
// incident or a bundle: those stay root-only, because they hold logs.

// readDiagFile decodes one file from Paths.DiagOut.
func (s *Server) readDiagFile(name string, v any) error {
	if s.Paths.DiagOut == "" {
		return errors.New("diagnostics are not configured")
	}
	b, err := os.ReadFile(filepath.Join(s.Paths.DiagOut, name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sibyl-diag has not written %s yet: sibyl-diag-metrics.service writes it every minute", name)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Server) getDiagnostics(w http.ResponseWriter, _ *http.Request) {
	var st diag.StatusFile
	if err := s.readDiagFile("status.json", &st); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if st.RecentIncidents == nil {
		st.RecentIncidents = []string{}
	}
	if st.Bundles == nil {
		st.Bundles = []diag.BundleInfo{}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) getDiagnosticsPerf(w http.ResponseWriter, _ *http.Request) {
	var p diag.PerfFile
	if err := s.readDiagFile("perf.json", &p); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if p.Models == nil {
		p.Models = []diag.ModelPerf{}
	}
	writeJSON(w, http.StatusOK, p)
}

// postDiagBundle makes a redacted bundle. The result message holds the
// diagnostic ID, and GET /diagnostics lists the bundle once it is made.
func (s *Server) postDiagBundle(w http.ResponseWriter, r *http.Request) {
	var b hostd.DiagBundleReq
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.hostAction(w, r, hostd.DiagBundle, b)
}

func (s *Server) postDiagSubmit(w http.ResponseWriter, r *http.Request) {
	s.hostAction(w, r, hostd.DiagSubmit, hostd.DiagSubmitReq{ID: r.PathValue("id")})
}

func (s *Server) putDiagConsent(w http.ResponseWriter, r *http.Request) {
	var c hostd.DiagConsentReq
	if err := readJSON(r, &c); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.hostAction(w, r, hostd.DiagConsent, c)
}

func (s *Server) putSafeMode(w http.ResponseWriter, r *http.Request) {
	var m hostd.SafeModeReq
	if err := readJSON(r, &m); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.hostAction(w, r, hostd.SetSafeMode, m)
}
