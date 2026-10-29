package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
	"github.com/diffsec/sibyl/internal/router"
	"go.yaml.in/yaml/v3"
)

// The router reads its configuration from config/router. The System API
// writes it, and the router reloads it within two seconds of a change.

func (p Paths) routerDir() string { return filepath.Join(p.Config, "router") }
func (p Paths) routerFile(name string) string {
	return filepath.Join(p.routerDir(), name)
}
func (p Paths) routerLog() string { return filepath.Join(p.routerDir(), "log", "decisions.jsonl") }

// writeRouterModels writes models.json, the local models with the context
// size that llama-server runs each one with.
func (s *Server) writeRouterModels(installed []models.Model, settings map[string]models.Settings, served map[string]bool) error {
	out := []router.LocalModel{}
	for _, m := range installed {
		if !served[m.ID] {
			continue
		}
		st := settings[m.ID]
		meta := s.modelMeta(m)
		lm := router.LocalModel{ID: m.ID, Context: st.CtxSize, Embedding: st.Embedding}
		// With several slots, each request is sure of its share only.
		if st.Parallel > 1 {
			lm.Context = st.CtxSize / st.Parallel
		}
		if lm.Context == 0 && meta != nil {
			lm.Context = meta.ContextLength
		}
		// The sampling of each preset, which a client picks per request
		// with <model>:<preset>. A request that names a preset asks for it,
		// so its values replace the model's defaults for that request.
		if !st.Embedding {
			lm.Presets = map[string]models.Sampling{}
			for _, p := range models.Presets {
				if ps, err := models.PresetSampling(p, meta); err == nil {
					lm.Presets[p] = ps
				}
			}
		}
		out = append(out, lm)
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Paths.routerDir(), 0o750); err != nil {
		return err
	}
	path := s.Paths.routerFile(router.ModelsName)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, body) {
		return nil
	}
	return hardware.WriteFileAtomic(path, body, 0o640)
}

// routerConfig reads the router configuration. It needs no provider keys,
// so it reads the files one by one.
type routerConfig struct {
	Clients   router.ClientsFile
	Providers router.ProvidersFile
	Profiles  router.ProfilesFile
}

func readYAMLFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}

func (s *Server) loadRouter() (routerConfig, error) {
	var c routerConfig
	for name, v := range map[string]any{
		router.ClientsName: &c.Clients, router.ProvidersName: &c.Providers, router.ProfilesName: &c.Profiles,
	} {
		if err := readYAMLFile(s.Paths.routerFile(name), v); err != nil {
			return c, fmt.Errorf("%s: %w", name, err)
		}
	}
	return c, nil
}

func (s *Server) saveYAML(name string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Paths.routerDir(), 0o750); err != nil {
		return err
	}
	return hardware.WriteFileAtomic(s.Paths.routerFile(name), b, 0o640)
}

type routerClientView struct {
	Name      string         `json:"name"`
	Policy    router.Policy  `json:"policy"`
	AllowHint bool           `json:"allow_hint"`
	Builtin   bool           `json:"builtin"`
	Budget    *router.Budget `json:"budget,omitempty"`
}

type routerProviderView struct {
	Name string `json:"name"`
	router.Provider
	KeySet bool `json:"key_set"`
}

type routerView struct {
	Clients   []routerClientView   `json:"clients"`
	Providers []routerProviderView `json:"providers"`
	Profiles  router.ProfilesFile  `json:"profiles"`
}

// clientList returns the clients with the built-in ones first. A built-in
// client that clients.yaml does not list is local-only.
func clientList(f router.ClientsFile) []routerClientView {
	var out []routerClientView
	for _, name := range []string{router.DefaultClient, router.OpenWebUIClient} {
		v := routerClientView{Name: name, Policy: router.LocalOnly, Builtin: true}
		for _, c := range f.Clients {
			if c.Name == name {
				v.Policy, v.AllowHint, v.Budget = c.Policy, c.AllowHint, c.Budget
			}
		}
		out = append(out, v)
	}
	for _, c := range f.Clients {
		if !router.Builtin(c.Name) {
			out = append(out, routerClientView{Name: c.Name, Policy: c.Policy, AllowHint: c.AllowHint, Budget: c.Budget})
		}
	}
	return out
}

func (s *Server) getRouter(w http.ResponseWriter, _ *http.Request) {
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	v := routerView{Clients: clientList(c.Clients), Providers: []routerProviderView{}, Profiles: c.Profiles}
	for name, p := range c.Providers.Providers {
		_, err := os.Stat(s.Paths.routerFile(filepath.Join(router.KeysDir, name)))
		v.Providers = append(v.Providers, routerProviderView{Name: name, Provider: p, KeySet: err == nil})
	}
	sort.Slice(v.Providers, func(i, j int) bool { return v.Providers[i].Name < v.Providers[j].Name })
	writeJSON(w, http.StatusOK, v)
}

type clientRequest struct {
	Policy    router.Policy  `json:"policy"`
	AllowHint bool           `json:"allow_hint"`
	Budget    *router.Budget `json:"budget,omitempty"`
}

// postRouterClient creates a client and returns its key. The key is shown
// once: the router stores only its hash.
func (s *Server) postRouterClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		clientRequest
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if router.Builtin(req.Name) {
		writeError(w, http.StatusConflict, fmt.Errorf("client %q is built in: change its policy with PUT", req.Name))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if slices.ContainsFunc(c.Clients.Clients, func(x router.Client) bool { return x.Name == req.Name }) {
		writeError(w, http.StatusConflict, fmt.Errorf("client %q exists", req.Name))
		return
	}
	key, hash, err := newClientKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if req.Policy == "" {
		req.Policy = router.LocalOnly
	}
	c.Clients.Clients = append(c.Clients.Clients, router.Client{
		Name: req.Name, KeySHA256: hash, Policy: req.Policy, AllowHint: req.AllowHint,
		Budget: req.Budget,
	})
	if err := s.saveClients(c.Clients); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.Log.Info("router client created", "client", req.Name, "policy", req.Policy)
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name, "policy": req.Policy, "key": key})
}

// newClientKey returns a new client key and its hex SHA-256.
func newClientKey() (key, hash string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = "sib-" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(key))
	return key, hex.EncodeToString(sum[:]), nil
}

// postRouterClientKey gives a client a new key. The old key stops working
// within two seconds, when the router reloads clients.yaml.
func (s *Server) postRouterClientKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if router.Builtin(name) {
		writeError(w, http.StatusConflict, fmt.Errorf("the key of %q is a Podman secret: replace it on the host with podman secret", name))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(c.Clients.Clients, func(x router.Client) bool { return x.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("no client %q", name))
		return
	}
	key, hash, err := newClientKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	c.Clients.Clients[i].KeySHA256 = hash
	if err := s.saveClients(c.Clients); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router client key replaced", "client", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "policy": c.Clients.Clients[i].Policy, "key": key})
}

func (s *Server) saveClients(f router.ClientsFile) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return s.saveYAML(router.ClientsName, f)
}

// putRouterClient changes a client's policy. It also sets the policy of a
// built-in client.
func (s *Server) putRouterClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req clientRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(c.Clients.Clients, func(x router.Client) bool { return x.Name == name })
	switch {
	case i >= 0:
		cl := &c.Clients.Clients[i]
		cl.Policy, cl.AllowHint, cl.Budget = req.Policy, req.AllowHint, req.Budget
	case router.Builtin(name):
		c.Clients.Clients = append(c.Clients.Clients, router.Client{Name: name, Policy: req.Policy,
			AllowHint: req.AllowHint, Budget: req.Budget})
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("no client %q", name))
		return
	}
	if err := s.saveClients(c.Clients); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.Log.Info("router client changed", "client", name, "policy", req.Policy)
	writeJSON(w, http.StatusOK, clientList(c.Clients))
}

func (s *Server) deleteRouterClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if router.Builtin(name) {
		writeError(w, http.StatusConflict, fmt.Errorf("client %q is built in and cannot be deleted", name))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	n := len(c.Clients.Clients)
	c.Clients.Clients = slices.DeleteFunc(c.Clients.Clients, func(x router.Client) bool { return x.Name == name })
	if len(c.Clients.Clients) == n {
		writeError(w, http.StatusNotFound, fmt.Errorf("no client %q", name))
		return
	}
	if err := s.saveClients(c.Clients); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router client deleted", "client", name)
	writeJSON(w, http.StatusOK, clientList(c.Clients))
}

// checkProviderKey asks the provider for its model list with the key, so a
// wrong key or URL fails here and not on the first request.
func (s *Server) checkProviderKey(ctx context.Context, p router.Provider, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := s.ProviderHTTP
	if c == nil {
		c = &http.Client{}
	}
	if p.Type == router.ProviderAnthropic {
		client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(p.URL()+"/"),
			option.WithAPIKey(key), option.WithHTTPClient(c), option.WithMaxRetries(0))
		if _, err := client.Models.List(ctx, anthropic.ModelListParams{Limit: anthropic.Int(1)}); err != nil {
			return fmt.Errorf("the provider refused the key: %w", err)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL()+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("reach the provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the provider refused the key: %s", resp.Status)
	}
	return nil
}

// putRouterProvider adds or changes a provider. A new provider needs a
// key. A change without a key keeps the stored one.
func (s *Server) putRouterProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		router.Provider
		APIKey string `json:"api_key"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if strings.ContainsAny(key, " \t\r\n") || len(key) > 1024 {
		writeError(w, http.StatusUnprocessableEntity, errors.New("api_key: paste the provider's API key"))
		return
	}
	keyFile := s.Paths.routerFile(filepath.Join(router.KeysDir, name))
	if key == "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, errors.New("api_key is required for a new provider"))
			return
		}
		key = strings.TrimSpace(string(b))
	}
	one := router.ProvidersFile{Providers: map[string]router.Provider{name: req.Provider}}
	if err := one.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := s.checkProviderKey(r.Context(), req.Provider, key); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if c.Providers.Providers == nil {
		c.Providers.Providers = map[string]router.Provider{}
	}
	c.Providers.Providers[name] = req.Provider
	// The key is written first: the router refuses a provider with no key.
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := hardware.WriteFileAtomic(keyFile, []byte(key+"\n"), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.saveYAML(router.ProvidersName, c.Providers); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router provider set", "provider", name, "base_url", req.BaseURL)
	s.getRouter(w, r)
}

func (s *Server) deleteRouterProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := c.Providers.Providers[name]; !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no provider %q", name))
		return
	}
	delete(c.Providers.Providers, name)
	// A profile that still names the provider would stop the router from
	// loading the new configuration.
	if err := c.Profiles.Validate(c.Providers); err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("change the profiles first: %w", err))
		return
	}
	if err := s.saveYAML(router.ProvidersName, c.Providers); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Remove(s.Paths.routerFile(filepath.Join(router.KeysDir, name))); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router provider deleted", "provider", name)
	s.getRouter(w, r)
}

// putRouterProfiles replaces profiles.yaml after it passes the router's
// own checks.
func (s *Server) putRouterProfiles(w http.ResponseWriter, r *http.Request) {
	var f router.ProfilesFile
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadRouter()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := f.Validate(c.Providers); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := s.saveYAML(router.ProfilesName, f); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router profiles set", "profiles", len(f.Profiles))
	s.getRouter(w, r)
}

// postRouterExplain asks the router how it would route a request. The
// router does not send the request.
func (s *Server) postRouterExplain(w http.ResponseWriter, r *http.Request) {
	s.askRouter(w, r, "/sibyl/explain")
}

// postRouterClassify asks the router's classifier for the labels of a
// text: {"text": "..."}.
func (s *Server) postRouterClassify(w http.ResponseWriter, r *http.Request) {
	s.askRouter(w, r, "/sibyl/classify")
}

// askRouter sends the request body to a router route that only the System
// API key can call, and relays the answer.
func (s *Server) askRouter(w http.ResponseWriter, r *http.Request, path string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RouterURL+path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("reach the router: %w", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
}

// getRouterExamples returns the owner's classifier examples and the
// number of starter examples.
func (s *Server) getRouterExamples(w http.ResponseWriter, _ *http.Request) {
	ex, err := readExamples(s.Paths.routerFile(router.ExamplesName))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"examples": ex, "starter": len(router.StarterExamples())})
}

// putRouterExamples replaces the owner's classifier examples with a JSON
// list of {text, task, difficulty}.
func (s *Server) putRouterExamples(w http.ResponseWriter, r *http.Request) {
	var ex []router.Example
	if err := readJSON(r, &ex); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var b bytes.Buffer
	for _, e := range ex {
		line, err := json.Marshal(e)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	// The router's parser is the check, so both sides agree.
	if _, err := router.ParseExamples(bytes.NewReader(b.Bytes())); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := os.MkdirAll(s.Paths.routerDir(), 0o750); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := hardware.WriteFileAtomic(s.Paths.routerFile(router.ExamplesName), b.Bytes(), 0o640); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("router examples set", "examples", len(ex))
	s.getRouterExamples(w, r)
}

func readExamples(path string) ([]router.Example, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []router.Example{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return router.ParseExamples(f)
}

// routerStats sums the decision log.
type routerStats struct {
	Requests int            `json:"requests"`
	ByClient map[string]int `json:"by_client"`
	ByRoute  map[string]int `json:"by_route"`
	Errors   int            `json:"errors"`
	// Unpriced counts provider requests whose cost is not known.
	Unpriced int                    `json:"unpriced"`
	Spend    map[string]clientSpend `json:"spend"`
	Last     []router.Entry         `json:"last"`
}

// clientSpend is a client's provider spend in US dollars, in UTC.
type clientSpend struct {
	Today     float64 `json:"today_usd"`
	ThisMonth float64 `json:"month_usd"`
}

func (p Paths) routerSpend() string { return filepath.Join(p.routerDir(), "log", "usage.json") }

// spend reads usage.json, which the router writes.
func (s *Server) spend() map[string]clientSpend {
	out := map[string]clientSpend{}
	b, err := os.ReadFile(s.Paths.routerSpend())
	if err != nil {
		return out
	}
	var f router.SpendFile
	if json.Unmarshal(b, &f) != nil {
		return out
	}
	clients := map[string]bool{}
	for _, day := range f.Days {
		for c := range day {
			clients[c] = true
		}
	}
	now := time.Now()
	for c := range clients {
		d, m := router.SpentIn(f.Days, c, now)
		out[c] = clientSpend{Today: d, ThisMonth: m}
	}
	return out
}

// getRouterStats reads the decision log: totals per client and per route
// since the log last rotated, and the last decisions.
func (s *Server) getRouterStats(w http.ResponseWriter, _ *http.Request) {
	st := routerStats{ByClient: map[string]int{}, ByRoute: map[string]int{}, Last: []router.Entry{}, Spend: s.spend()}
	f, err := os.Open(s.Paths.routerLog())
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, st)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = f.Close() }()
	const keep = 50
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e router.Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		st.Requests++
		st.ByClient[e.Client]++
		if e.Route != "" {
			st.ByRoute[e.Route]++
		}
		if e.Status >= 400 || e.Error != "" {
			st.Errors++
		}
		if e.Unpriced {
			st.Unpriced++
		}
		st.Last = append(st.Last, e)
		if len(st.Last) > keep {
			st.Last = st.Last[1:]
		}
	}
	slices.Reverse(st.Last)
	writeJSON(w, http.StatusOK, st)
}
