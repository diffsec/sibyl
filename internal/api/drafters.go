package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/hf"
	"github.com/diffsec/sibyl/internal/models"
)

// drafterTTL is how long a drafter search stays cached. New drafters
// appear on Hugging Face over days, not minutes.
const drafterTTL = 6 * time.Hour

// drafterSearchTimeout bounds one search: up to maxLineage card lookups
// and two searches for each repository in the lineage.
const drafterSearchTimeout = time.Minute

// drafterView is one drafter that the finder offers for a model.
type drafterView struct {
	hf.Drafter
	// Match is "model" when the drafter's card names the model or the
	// repository it was converted from, and "base" when it names a model
	// further up. A drafter for the base model accepts fewer tokens on a
	// finetune, and can make it slower.
	Match string `json:"match"`
	// Installed is the ID of the installed model from this repository.
	Installed string `json:"installed,omitempty"`
}

type draftersView struct {
	Model string `json:"model"`
	// Source is the repository that the search started from: the
	// download record, or else the file's general.base_model.
	Source   string         `json:"source,omitempty"`
	Lineage  map[string]int `json:"lineage,omitempty"`
	Drafters []drafterView  `json:"drafters"`
	Searched time.Time      `json:"searched"`
}

// drafterCache keeps the last search for each model.
type drafterCache struct {
	mu      sync.Mutex
	entries map[string]draftersView
	warming bool
}

func (c *drafterCache) get(id string) (draftersView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[id]
	return v, ok && time.Since(v.Searched) < drafterTTL
}

func (c *drafterCache) put(v draftersView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]draftersView{}
	}
	c.entries[v.Model] = v
}

// sourceRepo returns the repository that a downloaded model came from, from
// its .sibyl-source.json. A model copied in by hand has none.
func (s *Server) sourceRepo(m models.Model) string {
	dir := filepath.Dir(m.Path)
	if dir == "." {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(m.Root(s.Paths.Models, s.Paths.Pools), dir, ".sibyl-source.json"))
	if err != nil {
		return ""
	}
	var src struct {
		Repo string `json:"repo"`
	}
	if json.Unmarshal(raw, &src) != nil || !hf.ValidRepo(src.Repo) {
		return ""
	}
	return src.Repo
}

// findDrafters searches Hugging Face for drafters of model m. installed
// lists every installed model, to mark the drafters already downloaded.
func (s *Server) findDrafters(ctx context.Context, m models.Model, installed []models.Model) (draftersView, error) {
	v := draftersView{Model: m.ID, Drafters: []drafterView{}, Searched: time.Now()}
	lineage := map[string]int{}
	if v.Source = s.sourceRepo(m); v.Source != "" {
		lineage = s.HF.Lineage(ctx, v.Source)
	} else if meta := s.modelMeta(m); meta != nil {
		// The file's base model is one step above the file itself.
		for _, b := range meta.BaseRepos {
			if v.Source == "" {
				v.Source = b
			}
			for r, d := range s.HF.Lineage(ctx, b) {
				if old, ok := lineage[r]; !ok || d+1 < old {
					lineage[r] = d + 1
				}
			}
		}
	}
	v.Lineage = lineage
	byRepo := map[string]string{}
	for _, im := range installed {
		if r := s.sourceRepo(im); r != "" {
			byRepo[r] = im.ID
		}
	}
	seen := map[string]bool{}
	for base, depth := range lineage {
		found, err := s.HF.SearchDrafters(ctx, base, 20)
		if err != nil {
			return v, err
		}
		for _, d := range found {
			if seen[d.Repo] || d.Repo == v.Source {
				continue
			}
			seen[d.Repo] = true
			d.Depth = depth
			match := "base"
			if depth <= 1 {
				match = "model"
			}
			v.Drafters = append(v.Drafters, drafterView{Drafter: d, Match: match, Installed: byRepo[d.Repo]})
		}
	}
	slices.SortFunc(v.Drafters, func(a, b drafterView) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(b.Downloads, a.Downloads), cmp.Compare(a.Repo, b.Repo))
	})
	return v, nil
}

// getDrafters lists the drafters on Hugging Face for one model. A cached
// search is reused for drafterTTL, unless ?refresh=1.
func (s *Server) getDrafters(w http.ResponseWriter, r *http.Request) {
	if s.HF == nil {
		writeError(w, http.StatusServiceUnavailable, errString("no Hugging Face client"))
		return
	}
	id := r.PathValue("id")
	installed, err := s.scanModels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(installed, func(m models.Model) bool { return m.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, errString("model is not installed"))
		return
	}
	if installed[i].Drafter {
		writeError(w, http.StatusUnprocessableEntity, errString("the model is a drafter"))
		return
	}
	if v, ok := s.drafters.get(id); ok && r.URL.Query().Get("refresh") != "1" {
		writeJSON(w, http.StatusOK, v)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), drafterSearchTimeout)
	defer cancel()
	v, err := s.findDrafters(ctx, installed[i], installed)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.drafters.put(v)
	writeJSON(w, http.StatusOK, v)
}

// warmDrafters searches, one model at a time in the background, for the
// chat models with no fresh search, so that the model list can mark the
// models that have drafters. One warm-up runs at a time.
func (s *Server) warmDrafters(installed []models.Model, settings map[string]models.Settings) {
	var todo []models.Model
	for _, m := range installed {
		if _, ok := s.drafters.get(m.ID); !ok && !m.Drafter && !settings[m.ID].Embedding {
			todo = append(todo, m)
		}
	}
	s.drafters.mu.Lock()
	if len(todo) == 0 || s.drafters.warming || s.HF == nil {
		s.drafters.mu.Unlock()
		return
	}
	s.drafters.warming = true
	s.drafters.mu.Unlock()
	go func() {
		defer func() {
			s.drafters.mu.Lock()
			s.drafters.warming = false
			s.drafters.mu.Unlock()
		}()
		for _, m := range todo {
			ctx, cancel := context.WithTimeout(context.Background(), drafterSearchTimeout)
			v, err := s.findDrafters(ctx, m, installed)
			cancel()
			if err != nil {
				s.Log.Warn("drafter search", "model", m.ID, "err", err)
				continue
			}
			s.drafters.put(v)
		}
	}()
}
