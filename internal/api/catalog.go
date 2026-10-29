package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/catalog"
	"github.com/diffsec/sibyl/internal/hardware"
)

// Catalog keeps a local copy of the models.dev index for the router's
// prices and the provider setup. It fetches the index once a day. Only the
// host's address reaches models.dev: no key, model name or request.
type Catalog struct {
	URL  string // models.dev/api.json; empty turns the fetch off
	HTTP *http.Client
	// Every is how often the index is fetched again.
	Every time.Duration

	mu      sync.Mutex
	lastErr string
	lastTry time.Time
}

// maxCatalog is the largest index that is read. models.dev is about 6 MB.
const maxCatalog = 64 << 20

func (s *Server) catalogFile() string { return s.Paths.routerFile(catalog.FileName) }

// refreshCatalog fetches the index and writes the trimmed copy.
func (s *Server) refreshCatalog(ctx context.Context) (catalog.Catalog, error) {
	c := s.Catalog
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return catalog.Catalog{}, err
	}
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return catalog.Catalog{}, fmt.Errorf("fetch %s: %w", c.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return catalog.Catalog{}, fmt.Errorf("fetch %s: %s", c.URL, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalog))
	if err != nil {
		return catalog.Catalog{}, err
	}
	cat, err := catalog.Parse(raw, c.URL, time.Now())
	if err != nil {
		return catalog.Catalog{}, err
	}
	// A download that lost most providers is more likely a broken or
	// partial index than a real change, so the copy stays.
	if cur, err := catalog.Load(s.catalogFile()); err == nil && len(cat.Providers) < len(cur.Providers)/2 {
		return catalog.Catalog{}, fmt.Errorf("the new index has %d providers and the copy has %d: kept the copy",
			len(cat.Providers), len(cur.Providers))
	}
	b, err := json.Marshal(cat)
	if err != nil {
		return catalog.Catalog{}, err
	}
	if err := os.MkdirAll(s.Paths.routerDir(), 0o750); err != nil {
		return catalog.Catalog{}, err
	}
	if err := hardware.WriteFileAtomic(s.catalogFile(), b, 0o640); err != nil {
		return catalog.Catalog{}, err
	}
	s.Log.Info("models.dev index saved", "providers", len(cat.Providers))
	return cat, nil
}

func (s *Server) recordCatalog(err error) {
	s.Catalog.mu.Lock()
	defer s.Catalog.mu.Unlock()
	s.Catalog.lastTry, s.Catalog.lastErr = time.Now(), ""
	if err != nil {
		s.Catalog.lastErr = err.Error()
		s.Log.Warn("models.dev index", "err", err)
	}
}

// KeepCatalog fetches the index when the copy is missing or older than
// Every, and checks again every hour. A failed fetch keeps the old copy
// and tries again at the next check.
func (s *Server) KeepCatalog(ctx context.Context) {
	if s.Catalog == nil || s.Catalog.URL == "" {
		return
	}
	every := s.Catalog.Every
	if every <= 0 {
		every = 24 * time.Hour
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		cur, err := catalog.Load(s.catalogFile())
		if err != nil || time.Since(cur.Fetched) >= every {
			_, err := s.refreshCatalog(ctx)
			s.recordCatalog(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// catalogProvider is a provider in the GET /router/catalog list.
type catalogProvider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url"`
	Doc     string   `json:"doc,omitempty"`
	Env     []string `json:"env,omitempty"`
	Models  int      `json:"models"`
}

type catalogView struct {
	Fetched   string            `json:"fetched,omitempty"`
	Source    string            `json:"source,omitempty"`
	Enabled   bool              `json:"enabled"`
	Error     string            `json:"error,omitempty"`
	Providers []catalogProvider `json:"providers"`
}

func (s *Server) getCatalog(w http.ResponseWriter, _ *http.Request) {
	cat, err := catalog.Load(s.catalogFile())
	v := catalogView{Providers: []catalogProvider{}, Enabled: s.Catalog != nil && s.Catalog.URL != ""}
	if err != nil {
		v.Error = err.Error()
	}
	if s.Catalog != nil {
		s.Catalog.mu.Lock()
		if s.Catalog.lastErr != "" {
			v.Error = s.Catalog.lastErr
		}
		s.Catalog.mu.Unlock()
	}
	if !cat.Fetched.IsZero() {
		v.Fetched, v.Source = cat.Fetched.Format(time.RFC3339), cat.Source
	}
	for _, p := range cat.Sorted() {
		v.Providers = append(v.Providers, catalogProvider{ID: p.ID, Name: p.Name, Type: p.Type,
			BaseURL: p.BaseURL, Doc: p.Doc, Env: p.Env, Models: len(p.Models)})
	}
	writeJSON(w, http.StatusOK, v)
}

// getCatalogProvider returns one provider with its models. ?q= filters the
// models by ID or name.
func (s *Server) getCatalogProvider(w http.ResponseWriter, r *http.Request) {
	cat, err := catalog.Load(s.catalogFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	p, ok := cat.Providers[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no provider %q in the models.dev index", r.PathValue("id")))
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	models := []catalog.Model{}
	for _, m := range p.SortedModels() {
		if q == "" || strings.Contains(strings.ToLower(m.ID+" "+m.Name), q) {
			models = append(models, m)
		}
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name, "type": p.Type,
		"base_url": p.BaseURL, "doc": p.Doc, "env": p.Env, "models": models})
}

// postCatalogRefresh fetches the index now.
func (s *Server) postCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if s.Catalog == nil || s.Catalog.URL == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("the models.dev index is turned off"))
		return
	}
	_, err := s.refreshCatalog(r.Context())
	s.recordCatalog(err)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.getCatalog(w, r)
}
