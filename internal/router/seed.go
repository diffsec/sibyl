package router

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// heldoutJSONL is the labelled request set of step 6.3. None of its
// requests is a starter example. sibyl-eval reads it, and the router
// sends it to a new local model as its first calibration.
//
//go:embed heldout.jsonl
var heldoutJSONL []byte

// HeldOut returns the labelled request set as JSON lines.
func HeldOut() []byte { return heldoutJSONL }

// HeldOutItem is one request of the labelled set.
type HeldOutItem struct {
	Prompt       string `json:"prompt"`
	Task         string `json:"task"`
	Difficulty   string `json:"difficulty"`
	OutputTokens int    `json:"output_tokens"`
}

func heldOutItems() []HeldOutItem {
	var out []HeldOutItem
	sc := bufio.NewScanner(bytes.NewReader(heldoutJSONL))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var it HeldOutItem
		if json.Unmarshal([]byte(line), &it) == nil && it.Prompt != "" {
			out = append(out, it)
		}
	}
	return out
}

// The seed samples of a new local model.
const (
	// SeedClient is the client name of a seed sample. It is not a valid
	// client name, so no client shares it.
	SeedClient = "@seed"
	// SeededName is the file in the calibration directory that lists the
	// local models that got their seed samples.
	SeededName = "seeded.json"
	// seedMaxTokens limits a seed answer. A hard request needs room.
	seedMaxTokens = 8192
)

// seed writes the labelled set as samples for each profile's local model
// that has not had them. The worker then gets both answers and a verdict
// for each, so a new local model starts with outcomes. It runs only when
// a client calibrates.
func (s *Server) seed(cfg *Config) {
	cc := cfg.Profiles.Calibration
	if s.CalibrationDir == "" || cc.NoSeed || !cfg.calibrates() {
		return
	}
	path := filepath.Join(s.CalibrationDir, SeededName)
	seeded := map[string]time.Time{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &seeded)
	}
	now := time.Now()
	changed := false
	for _, name := range sortedKeys(cfg.Profiles.Profiles) {
		model := cfg.Profiles.Profiles[name].LocalModel
		if _, done := seeded[model]; done {
			continue
		}
		for i, it := range heldOutItems() {
			body, err := json.Marshal(map[string]any{
				"model":      "auto",
				"max_tokens": seedMaxTokens,
				"messages":   []map[string]string{{"role": "user", "content": it.Prompt}},
			})
			if err != nil {
				continue
			}
			t := now.Add(time.Duration(i))
			smp := Sample{ID: sampleID(t), Time: t.UTC(), Client: SeedClient, Profile: name,
				Task: it.Task, Difficulty: it.Difficulty, LocalModel: model, Served: SeedClient, Request: body}
			if err := s.writeSample(smp); err != nil {
				s.logger().Warn("write a seed sample", "err", err)
				return
			}
		}
		seeded[model], changed = now.UTC(), true
		s.logger().Info("calibration seeded", "local_model", model, "profile", name)
	}
	if !changed {
		return
	}
	if b, err := json.Marshal(seeded); err == nil {
		_ = writeFileAtomic(path, b)
	}
}
