package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/diffsec/sibyl/internal/router"
)

func (p Paths) calibrationDir() string { return filepath.Join(p.routerDir(), "calibration") }

// calibrationView is the calibration report with the worker's status, the
// day's calibration spend and the rule proposals.
type calibrationView struct {
	router.CalibrationReport
	Status        *router.CalibrationStatus `json:"status,omitempty"`
	SpentTodayUSD float64                   `json:"spent_today_usd"`
	DailyUSD      float64                   `json:"daily_usd"`
	AutoApply     bool                      `json:"auto_apply"`
	Proposals     []router.Proposal         `json:"proposals"`
}

// calibration reads the samples and the router configuration, and makes
// the proposals.
func (s *Server) calibration() (calibrationView, *router.Config, error) {
	dir := s.Paths.calibrationDir()
	v := calibrationView{
		CalibrationReport: router.Summarize(router.LoadSamples(dir)),
		Status:            router.LoadStatus(dir),
		SpentTodayUSD:     s.spend()[router.CalibrationSpender].Today,
		Proposals:         []router.Proposal{},
	}
	cfg, err := router.LoadConfig(s.Paths.routerDir())
	if err != nil {
		return v, nil, err
	}
	v.DailyUSD, v.AutoApply = cfg.Profiles.Calibration.Daily(), cfg.Profiles.Calibration.AutoApply
	v.Proposals = router.Propose(cfg, v.CalibrationReport)
	return v, cfg, nil
}

// getRouterCalibration reports the local win-or-tie rate per local model,
// task and difficulty from the judged samples, and the rule proposals.
func (s *Server) getRouterCalibration(w http.ResponseWriter, _ *http.Request) {
	v, _, err := s.calibration()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteRouterCalibration deletes every calibration sample. The samples
// are the only message content that Sibyl keeps.
func (s *Server) deleteRouterCalibration(w http.ResponseWriter, _ *http.Request) {
	n, err := router.ClearSamples(s.Paths.calibrationDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("calibration samples deleted", "samples", n)
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

var errNoProposal = errors.New("no such proposal: read GET /router/calibration again")

// applyProposals adds the rules of the proposals that pick accepts to the
// profiles, and saves them. It returns the applied proposals.
func (s *Server) applyProposals(pick func(router.Proposal) bool) ([]router.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, cfg, err := s.calibration()
	if err != nil {
		return nil, err
	}
	f := cfg.Profiles
	f.Profiles = make(map[string]router.Profile, len(cfg.Profiles.Profiles))
	for name, p := range cfg.Profiles.Profiles {
		p.Rules = slices.Clone(p.Rules)
		f.Profiles[name] = p
	}
	var applied []router.Proposal
	for _, prop := range v.Proposals {
		if !pick(prop) {
			continue
		}
		if err := router.Apply(&f, prop); err != nil {
			return nil, err
		}
		applied = append(applied, prop)
	}
	if len(applied) == 0 {
		return nil, nil
	}
	if err := f.Validate(cfg.Providers); err != nil {
		return nil, err
	}
	if err := s.saveYAML(router.ProfilesName, f); err != nil {
		return nil, err
	}
	for _, prop := range applied {
		s.Log.Info("calibration rule applied", "profile", prop.Profile, "rule", prop.Rule.Name,
			"from", prop.Current, "to", prop.Route, "comparisons", prop.Comparisons, "local_ok_rate", prop.LocalOKRate)
	}
	return applied, nil
}

// postRouterCalibrationApply applies one proposal: {"id": "..."}.
func (s *Server) postRouterCalibrationApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("give the proposal's id: %v", err))
		return
	}
	applied, err := s.applyProposals(func(p router.Proposal) bool { return p.ID == req.ID })
	switch {
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err)
	case len(applied) == 0:
		writeError(w, http.StatusNotFound, errNoProposal)
	default:
		writeJSON(w, http.StatusOK, applied[0])
	}
}

// KeepCalibration applies the allowed proposals every interval while the
// profiles set auto_apply. Each change goes to the log.
func (s *Server) KeepCalibration(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := router.LoadConfig(s.Paths.routerDir())
		if err != nil || !cfg.Profiles.Calibration.AutoApply {
			continue
		}
		if _, err := s.applyProposals(func(p router.Proposal) bool { return p.Allowed }); err != nil {
			s.Log.Warn("calibration proposals not applied", "err", err)
		}
	}
}
