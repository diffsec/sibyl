package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// LoadSamples reads the calibration samples in dir, oldest first. A file
// that does not parse is left out.
func LoadSamples(dir string) []Sample {
	var out []Sample
	for _, n := range sampleFiles(dir) {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		var smp Sample
		if json.Unmarshal(b, &smp) == nil {
			out = append(out, smp)
		}
	}
	return out
}

// LoadStatus reads the calibration worker's status, if it wrote one.
func LoadStatus(dir string) *CalibrationStatus {
	b, err := os.ReadFile(filepath.Join(dir, StatusName))
	if err != nil {
		return nil
	}
	var st CalibrationStatus
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// ClearSamples deletes every sample and the status file in dir. It returns
// the number of samples deleted.
func ClearSamples(dir string) (int, error) {
	n := 0
	for _, name := range sampleFiles(dir) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return n, err
		}
		n++
	}
	if err := os.Remove(filepath.Join(dir, StatusName)); err != nil && !os.IsNotExist(err) {
		return n, err
	}
	return n, nil
}

// CalibrationCell is the result for one local model, task and difficulty.
type CalibrationCell struct {
	LocalModel string `json:"local_model"`
	Task       string `json:"task"`
	Difficulty string `json:"difficulty"`
	// Comparisons counts the judged samples.
	Comparisons int `json:"comparisons"`
	LocalWins   int `json:"local_wins"`
	Ties        int `json:"ties"`
	// LocalOKRate is the share of comparisons where the local answer won
	// or tied.
	LocalOKRate float64 `json:"local_ok_rate"`
	// JudgeIsSide counts the verdicts where the judge wrote one answer.
	JudgeIsSide int `json:"judge_is_side,omitempty"`
	// ProviderCostUSD is the mean provider cost of one answer, from the
	// answers that have a price.
	ProviderCostUSD   float64 `json:"provider_cost_usd"`
	LocalLatencyMS    int64   `json:"local_latency_ms"`
	ProviderLatencyMS int64   `json:"provider_latency_ms"`
}

// CalibrationReport sums up the calibration samples.
type CalibrationReport struct {
	Samples int `json:"samples"`
	// Pending samples still need an answer or a verdict.
	Pending int `json:"pending"`
	Judged  int `json:"judged"`
	// Failed samples failed three times and are left alone.
	Failed int               `json:"failed"`
	Oldest *time.Time        `json:"oldest,omitempty"`
	Cells  []CalibrationCell `json:"cells"`
}

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	slices.Sort(xs)
	return xs[len(xs)/2]
}

// Summarize counts the samples and the verdicts per local model, task and
// difficulty.
func Summarize(samples []Sample) CalibrationReport {
	r := CalibrationReport{Samples: len(samples), Cells: []CalibrationCell{}}
	type acc struct {
		cell            CalibrationCell
		costs           []float64
		local, provider []int64
	}
	cells := map[[3]string]*acc{}
	for i, smp := range samples {
		if i == 0 {
			t := smp.Time
			r.Oldest = &t
		}
		switch {
		case smp.Verdict != nil:
			r.Judged++
		case smp.Attempts >= maxSampleAttempts:
			r.Failed++
			continue
		default:
			r.Pending++
			continue
		}
		key := [3]string{smp.LocalModel, orUnknown(smp.Task), orUnknown(smp.Difficulty)}
		a := cells[key]
		if a == nil {
			a = &acc{cell: CalibrationCell{LocalModel: key[0], Task: key[1], Difficulty: key[2]}}
			cells[key] = a
		}
		v := smp.Verdict
		a.cell.Comparisons++
		switch v.Winner {
		case WinLocal:
			a.cell.LocalWins++
		case WinTie:
			a.cell.Ties++
		}
		if v.JudgeIsSide {
			a.cell.JudgeIsSide++
		}
		if smp.Local != nil {
			a.local = append(a.local, smp.Local.LatencyMS)
		}
		if smp.Provider != nil {
			a.provider = append(a.provider, smp.Provider.LatencyMS)
			if smp.Provider.CostUSD > 0 {
				a.costs = append(a.costs, smp.Provider.CostUSD)
			}
		}
	}
	for _, a := range cells {
		c := a.cell
		c.LocalOKRate = float64(c.LocalWins+c.Ties) / float64(c.Comparisons)
		c.LocalLatencyMS, c.ProviderLatencyMS = median(a.local), median(a.provider)
		if len(a.costs) > 0 {
			sum := 0.0
			for _, x := range a.costs {
				sum += x
			}
			c.ProviderCostUSD = sum / float64(len(a.costs))
		}
		r.Cells = append(r.Cells, c)
	}
	slices.SortFunc(r.Cells, func(x, y CalibrationCell) int {
		return strings.Compare(x.LocalModel+"\x00"+x.Task+"\x00"+x.Difficulty, y.LocalModel+"\x00"+y.Task+"\x00"+y.Difficulty)
	})
	return r
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
