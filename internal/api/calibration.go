package api

import (
	"net/http"
	"path/filepath"

	"github.com/diffsec/sibyl/internal/router"
)

func (p Paths) calibrationDir() string { return filepath.Join(p.routerDir(), "calibration") }

// calibrationView is the calibration report with the worker's status and
// the day's calibration spend.
type calibrationView struct {
	router.CalibrationReport
	Status        *router.CalibrationStatus `json:"status,omitempty"`
	SpentTodayUSD float64                   `json:"spent_today_usd"`
	DailyUSD      float64                   `json:"daily_usd"`
}

// getRouterCalibration reports the local win-or-tie rate per local model,
// task and difficulty from the judged samples.
func (s *Server) getRouterCalibration(w http.ResponseWriter, _ *http.Request) {
	dir := s.Paths.calibrationDir()
	v := calibrationView{
		CalibrationReport: router.Summarize(router.LoadSamples(dir)),
		Status:            router.LoadStatus(dir),
		SpentTodayUSD:     s.spend()[router.CalibrationSpender].Today,
		DailyUSD:          router.DefaultCalibUSD,
	}
	if c, err := s.loadRouter(); err == nil {
		v.DailyUSD = c.Profiles.Calibration.Daily()
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
