package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBody is the largest request body. Images in a request are base64, so
// it is large.
const maxBody = 64 << 20

// Server is the router's HTTP server.
type Server struct {
	// ConfigDir holds clients.yaml, providers.yaml, profiles.yaml,
	// models.json and keys/.
	ConfigDir string
	// LocalURL is llama-swap, such as http://llama-swap:8080.
	LocalURL string
	// AdminKey is the System API key. It is also the key of the client
	// "default", and it is the only key that can call /sibyl/explain.
	AdminKey string
	// OpenWebUIKey is the key of the client "open-webui".
	OpenWebUIKey string
	// HTTP sends requests to llama-swap and the providers. It has no
	// overall timeout, because responses stream.
	HTTP   *http.Client
	Log    *Log
	Logger *slog.Logger
	// Spend keeps the provider spend per client, for the budgets.
	Spend *Spend

	mu    sync.RWMutex
	cfg   *Config
	stamp string
}

// config returns the current configuration.
func (s *Server) config() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil {
		return &Config{Keys: map[string]string{}}
	}
	return s.cfg
}

// stampOf describes the configuration files by name, size and time, so a
// change shows without reading them.
func stampOf(dir string) string {
	var b strings.Builder
	names := []string{ClientsName, ProvidersName, ProfilesName, ModelsName}
	if es, err := os.ReadDir(filepath.Join(dir, KeysDir)); err == nil {
		for _, e := range es {
			names = append(names, filepath.Join(KeysDir, e.Name()))
		}
	}
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", n, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// Reload reads the configuration if it changed. A configuration with an
// error is not used: the router keeps the last good one.
func (s *Server) Reload() error {
	stamp := stampOf(s.ConfigDir)
	s.mu.RLock()
	same := s.cfg != nil && stamp == s.stamp
	s.mu.RUnlock()
	if same {
		return nil
	}
	cfg, err := LoadConfig(s.ConfigDir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg, s.stamp = cfg, stamp
	s.mu.Unlock()
	s.logger().Info("configuration loaded", "clients", len(cfg.Clients.Clients),
		"providers", len(cfg.Providers.Providers), "profiles", len(cfg.Profiles.Profiles))
	return nil
}

// Watch reloads the configuration every interval until ctx ends.
func (s *Server) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reload(); err != nil {
				if err.Error() != last {
					s.logger().Error("configuration not loaded; keeping the last good one", "err", err)
				}
				last = err.Error()
			} else {
				last = ""
			}
		}
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Handler returns the router's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", s.authed(s.models))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	mux.HandleFunc("POST /v1/messages", s.authedMsg(s.messages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.authedMsg(s.countTokens))
	mux.HandleFunc("POST /v1/embeddings", s.authed(s.embeddings))
	mux.HandleFunc("/v1/", s.authed(s.passthrough))
	mux.HandleFunc("POST /sibyl/explain", s.explain)
	return mux
}

// clientKey reads the key from Authorization: Bearer, or from x-api-key as
// Anthropic clients send it.
func clientKey(r *http.Request) string {
	if k, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(k)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// client returns the client that owns key.
func (s *Server) client(key string) (Client, bool) {
	cfg := s.config()
	builtin := func(name string) Client {
		for _, c := range cfg.Clients.Clients {
			if c.Name == name {
				return c
			}
		}
		return Client{Name: name, Policy: LocalOnly}
	}
	if key == "" {
		return Client{}, false
	}
	if equal(key, s.AdminKey) {
		return builtin(DefaultClient), true
	}
	if equal(key, s.OpenWebUIKey) {
		return builtin(OpenWebUIClient), true
	}
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	var found Client
	ok := false
	// Compare every hash, so the time does not depend on which client matches.
	for _, c := range cfg.Clients.Clients {
		if c.KeySHA256 != "" && equal(h, c.KeySHA256) {
			found, ok = c, true
		}
	}
	return found, ok
}

type ctxKey struct{}

func clientOf(r *http.Request) Client { c, _ := r.Context().Value(ctxKey{}).(Client); return c }

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.client(clientKey(r))
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	}
}

func (s *Server) authedMsg(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.client(clientKey(r))
		if !ok {
			msgError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	typ := "invalid_request_error"
	if code >= 500 {
		typ = "server_error"
	}
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

// readBody reads the request body up to maxBody.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, refuse(http.StatusRequestEntityTooLarge, "the request body is over %d MB", maxBody>>20)
	}
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "read the request body: %v", err)
	}
	return b, nil
}

func errCode(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.code
	}
	return http.StatusBadRequest
}

// resolver returns the decision inputs for a client and request.
func (s *Server) resolver(c Client, h http.Header) resolver {
	return resolver{cfg: s.config(), client: c, hint: hint(h, c), ready: s.localReady}
}

// localReady asks llama-swap whether model is loaded and ready.
func (s *Server) localReady(model string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.LocalURL+"/running", nil)
	if err != nil {
		return false
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&running) != nil {
		return false
	}
	for _, m := range running.Running {
		if m.Model == model && m.State == "ready" {
			return true
		}
	}
	return false
}

// withModel returns body with its model field set to model.
func withModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	mb, _ := json.Marshal(model)
	m["model"] = mb
	return json.Marshal(m)
}

// cancelBody cancels the request context when the response body closes.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// try sends one request to a route. path is the endpoint after /v1, such
// as /chat/completions. wait, when over 0, limits the time until the
// response headers arrive.
func (s *Server) try(ctx context.Context, cfg *Config, rt Route, path string, body []byte, in http.Header, wait time.Duration) (*http.Response, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	var timer *time.Timer
	if wait > 0 {
		timer = time.AfterFunc(wait, cancel)
	}
	resp, err := s.do(reqCtx, cfg, rt, path, body, in)
	if timer != nil && !timer.Stop() {
		if err == nil {
			_ = resp.Body.Close()
		}
		cancel()
		return nil, fmt.Errorf("%s did not answer within %s", rt, wait)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelBody{resp.Body, cancel}
	return resp, nil
}

// do sends one request in the API of the route's provider.
func (s *Server) do(ctx context.Context, cfg *Config, rt Route, path string, body []byte, in http.Header) (*http.Response, error) {
	var target string
	header := http.Header{"Content-Type": {"application/json"}}
	if a := in.Get("Accept"); a != "" {
		header.Set("Accept", a)
	}
	switch p := cfg.Providers.Providers[rt.Provider]; {
	case rt.Local():
		target = s.LocalURL + "/v1" + path
	case p.Type == ProviderAnthropic:
		switch path {
		case "/chat/completions":
			return s.anthropicChatCall(ctx, cfg, rt, body)
		case "/messages":
			return s.anthropicPassthrough(ctx, cfg, rt, body, in)
		}
		return nil, refuse(http.StatusBadRequest, "%s has no %s endpoint", rt.Provider, path)
	default:
		target = p.URL() + path
		header.Set("Authorization", "Bearer "+cfg.Keys[rt.Provider])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	return s.HTTP.Do(req)
}

// retryAfter reads a Retry-After header in seconds, up to 10 seconds.
func retryAfter(h http.Header) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n < 0 {
		return time.Second
	}
	return min(time.Duration(n)*time.Second, 10*time.Second)
}

func retryable(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }

// send tries the routes of d in order. It returns the first response that
// is not a failure, or the last response or error. build makes the body and
// the endpoint path for a route.
func (s *Server) send(r *http.Request, c Client, d Decision, build func(Route) ([]byte, string, error)) (*http.Response, Route, error) {
	cfg := s.config()
	local := staysLocal(c, hint(r.Header, c))
	attempts := d.Attempts()
	// A client whose budget is spent can still use the profile's local
	// model, even when the fallback list does not name it.
	day, month := s.Spend.Spent(c.Name, time.Now())
	spent := overBudget(c.Budget, day, month)
	if spent != "" && d.LocalModel != "" && !slices.ContainsFunc(attempts, Route.Local) {
		attempts = append(attempts, Route{Model: d.LocalModel})
	}
	var lastErr error
	for i, rt := range attempts {
		// The rules already keep a local-only request on the host. This
		// check holds even if a rule or a fallback list is wrong.
		if local && !rt.Local() {
			lastErr = refuse(http.StatusForbidden, "client %q must stay on this host", c.Name)
			continue
		}
		if !rt.Local() {
			if _, ok := cfg.Providers.Providers[rt.Provider]; !ok {
				lastErr = refuse(http.StatusNotFound, "no provider %q", rt.Provider)
				continue
			}
			if spent != "" {
				lastErr = refuse(http.StatusTooManyRequests, "client %q: %s", c.Name, spent)
				continue
			}
			if _, priced := cfg.price(rt); c.Budget.set() && !priced {
				lastErr = refuse(http.StatusTooManyRequests, "client %q has a budget, and %s has no price, so its cost cannot be counted", c.Name, rt)
				continue
			}
		}
		body, path, err := build(rt)
		if err != nil {
			return nil, rt, refuse(http.StatusBadRequest, "%v", err)
		}
		next := i < len(attempts)-1
		wait := time.Duration(0)
		if rt.Local() && next {
			wait = d.LocalWait
		}
		tries := 1
		if !rt.Local() {
			tries = 2
		}
		for t := 0; t < tries; t++ {
			resp, err := s.try(r.Context(), cfg, rt, path, body, r.Header, wait)
			switch {
			case err != nil:
				lastErr = err
				s.logger().Warn("route failed", "route", rt.String(), "err", err)
			case retryable(resp.StatusCode) && (next || t+1 < tries):
				lastErr = fmt.Errorf("%s returned %d", rt, resp.StatusCode)
				s.logger().Warn("route failed", "route", rt.String(), "status", resp.StatusCode)
				delay := retryAfter(resp.Header)
				_ = resp.Body.Close()
				if t+1 < tries {
					select {
					case <-time.After(delay):
					case <-r.Context().Done():
						return nil, rt, r.Context().Err()
					}
				}
				continue
			default:
				return resp, rt, nil
			}
			break
		}
	}
	if lastErr == nil {
		lastErr = refuse(http.StatusBadGateway, "no route is available")
	}
	var se *statusError
	if !errors.As(lastErr, &se) {
		lastErr = refuse(http.StatusBadGateway, "%v", lastErr)
	}
	return nil, Route{}, lastErr
}

// routeHeader is the X-Sibyl-Route response header.
func routeHeader(rt Route, d Decision) string {
	if d.Profile == "" {
		return rt.String()
	}
	return fmt.Sprintf("%s; profile=%q; rule=%q", rt, d.Profile, d.Rule)
}

// wireUsage holds the usage fields of both formats: the OpenAI chat format,
// and the Anthropic Messages format, where message_start nests it under
// "message".
type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type usageDoc struct {
	Usage   *wireUsage `json:"usage"`
	Message struct {
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
}

// merge adds a usage report to u. Streams report usage in parts, and
// Anthropic repeats some counts, so each field keeps its largest value.
func (u *Usage) merge(w *wireUsage) {
	if w == nil {
		return
	}
	if w.InputTokens+w.OutputTokens+w.CacheReadInputTokens > 0 && w.PromptTokens == 0 {
		// Anthropic: input_tokens excludes the cached tokens.
		u.Prompt = max(u.Prompt, w.InputTokens+w.CacheReadInputTokens+w.CacheCreationInputTokens)
		u.Completion = max(u.Completion, w.OutputTokens)
	} else {
		u.Prompt = max(u.Prompt, w.PromptTokens)
		u.Completion = max(u.Completion, w.CompletionTokens)
	}
	u.CacheRead = max(u.CacheRead, w.PromptTokensDetails.CachedTokens, w.CacheReadInputTokens)
	u.CacheWrite = max(u.CacheWrite, w.CacheCreationInputTokens)
}

// usageTap reads the token usage from a response as it is copied.
type usageTap struct {
	sse   bool
	line  []byte
	buf   bytes.Buffer
	usage Usage
}

const maxTapBody = 8 << 20

func (u *usageTap) Write(p []byte) (int, error) {
	if !u.sse {
		if u.buf.Len()+len(p) <= maxTapBody {
			u.buf.Write(p)
		}
		return len(p), nil
	}
	for _, c := range p {
		if c != '\n' {
			if len(u.line) < maxTapBody {
				u.line = append(u.line, c)
			}
			continue
		}
		if data, ok := bytes.CutPrefix(u.line, []byte("data:")); ok && bytes.Contains(data, []byte(`"usage"`)) {
			var d usageDoc
			if json.Unmarshal(bytes.TrimSpace(data), &d) == nil {
				u.usage.merge(d.Usage)
				u.usage.merge(d.Message.Usage)
			}
		}
		u.line = u.line[:0]
	}
	return len(p), nil
}

func (u *usageTap) result() Usage {
	if !u.sse {
		var d usageDoc
		if json.Unmarshal(u.buf.Bytes(), &d) == nil {
			u.usage.merge(d.Usage)
		}
	}
	return u.usage
}

// relay copies an upstream response to the client, flushing as it goes.
func relay(w http.ResponseWriter, resp *http.Response, routeHdr string) (Usage, error) {
	for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control", "Retry-After", "X-Sibyl-Dropped"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Sibyl-Route", routeHdr)
	w.WriteHeader(resp.StatusCode)
	tap := &usageTap{sse: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = tap.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return tap.result(), werr
			}
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			return tap.result(), nil
		}
		if err != nil {
			return tap.result(), err
		}
	}
}

// record writes the decision log entry. For a provider route it adds the
// cost to the client's spend.
func (s *Server) record(e Entry, start time.Time) {
	e.Time = start.UTC()
	e.LatencyMS = time.Since(start).Milliseconds()
	s.Log.Add(e)
	if e.CostUSD > 0 {
		if err := s.Spend.Add(e.Client, e.CostUSD, start); err != nil {
			s.logger().Error("record the spend", "client", e.Client, "err", err)
		}
	}
}

// account fills the route, usage and cost of an entry.
func (s *Server) account(e *Entry, rt Route, u Usage) {
	e.Route = rt.String()
	e.PromptTokens, e.CompletionTokens = u.Prompt, u.Completion
	e.CacheReadTokens, e.CacheWriteTokens = u.CacheRead, u.CacheWrite
	if rt.Local() {
		return
	}
	if p, ok := s.config().price(rt); ok {
		e.CostUSD = p.Cost(u)
	} else {
		e.Unpriced = true
	}
}

func entryFor(c Client, endpoint string, d Decision) Entry {
	return Entry{Client: c.Name, Endpoint: endpoint, Requested: d.Requested, Profile: d.Profile, Rule: d.Rule}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	c := clientOf(r)
	e := Entry{Client: c.Name, Endpoint: "chat"}
	fail := func(err error) {
		e.Status, e.Error = errCode(err), err.Error()
		s.record(e, start)
		writeOpenAIError(w, e.Status, err.Error())
	}
	body, err := readBody(w, r)
	if err != nil {
		fail(err)
		return
	}
	req, err := parseChat(body)
	if err != nil {
		fail(err)
		return
	}
	e.Requested = req.Model
	d, err := s.resolver(c, r.Header).decide(req.Model, req.features(s.config().Profiles.SensitiveWords))
	if err != nil {
		fail(err)
		return
	}
	e = entryFor(c, "chat", d)
	resp, rt, err := s.send(r, c, d, func(rt Route) ([]byte, string, error) {
		b, err := withModel(body, rt.Model)
		return b, "/chat/completions", err
	})
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.Status = resp.StatusCode
	u, err := relay(w, resp, routeHeader(rt, d))
	s.account(&e, rt, u)
	if err != nil {
		e.Error = err.Error()
	}
	s.record(e, start)
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	c := clientOf(r)
	e := Entry{Client: c.Name, Endpoint: "embeddings"}
	fail := func(err error) {
		e.Status, e.Error = errCode(err), err.Error()
		s.record(e, start)
		writeOpenAIError(w, e.Status, err.Error())
	}
	body, err := readBody(w, r)
	if err != nil {
		fail(err)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		fail(refuse(http.StatusBadRequest, "model is required"))
		return
	}
	e.Requested = req.Model
	if _, isProfile := profileName(req.Model); isProfile {
		fail(refuse(http.StatusBadRequest, "routing profiles apply to chat requests only: name an embedding model"))
		return
	}
	d, err := s.resolver(c, r.Header).decide(req.Model, Features{})
	if err != nil {
		fail(err)
		return
	}
	resp, rt, err := s.send(r, c, d, func(rt Route) ([]byte, string, error) {
		b, err := withModel(body, rt.Model)
		return b, "/embeddings", err
	})
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.Status = resp.StatusCode
	u, err := relay(w, resp, routeHeader(rt, d))
	s.account(&e, rt, u)
	if err != nil {
		e.Error = err.Error()
	}
	s.record(e, start)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	c := clientOf(r)
	e := Entry{Client: c.Name, Endpoint: "messages"}
	fail := func(err error) {
		e.Status, e.Error = errCode(err), err.Error()
		s.record(e, start)
		msgError(w, e.Status, err.Error())
	}
	body, err := readBody(w, r)
	if err != nil {
		fail(err)
		return
	}
	req, err := parseMessages(body)
	if err != nil {
		fail(refuse(http.StatusBadRequest, "%v", err))
		return
	}
	e.Requested = req.Model
	d, err := s.resolver(c, r.Header).decide(req.Model, req.features(s.config().Profiles.SensitiveWords))
	if err != nil {
		fail(err)
		return
	}
	e = entryFor(c, "messages", d)
	cfg := s.config()
	anthropicRoute := func(rt Route) bool {
		return !rt.Local() && cfg.Providers.Providers[rt.Provider].Type == ProviderAnthropic
	}
	resp, rt, err := s.send(r, c, d, func(rt Route) ([]byte, string, error) {
		// An anthropic route gets the client's request as it came.
		if anthropicRoute(rt) {
			return body, "/messages", nil
		}
		b, err := json.Marshal(req.toChat(rt.Model, rt.Local()))
		return b, "/chat/completions", err
	})
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.Status = resp.StatusCode
	if anthropicRoute(rt) {
		u, err := relay(w, resp, routeHeader(rt, d))
		s.account(&e, rt, u)
		if err != nil {
			e.Error = err.Error()
		}
		s.record(e, start)
		return
	}
	w.Header().Set("X-Sibyl-Route", routeHeader(rt, d))
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		e.Error = upstreamMessage(b)
		e.Route = rt.String()
		s.record(e, start)
		msgError(w, resp.StatusCode, fmt.Sprintf("%s: %s", rt, e.Error))
		return
	}
	var cu chatUsage
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		cu, err = streamMessages(w, resp.Body, req.Model)
	} else {
		var b []byte
		var out map[string]any
		if b, err = io.ReadAll(io.LimitReader(resp.Body, maxTapBody)); err == nil {
			out, cu, err = toMessage(b, req.Model)
		}
		if err != nil {
			fail(refuse(http.StatusBadGateway, "%v", err))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
	s.account(&e, rt, Usage{Prompt: cu.PromptTokens, Completion: cu.CompletionTokens})
	if err != nil {
		e.Error = err.Error()
	}
	s.record(e, start)
}

// countTokens estimates the input tokens of a Messages request, as the
// rules count them. No model runs.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		msgError(w, errCode(err), err.Error())
		return
	}
	var req msgRequest
	if err := json.Unmarshal(body, &req); err != nil {
		msgError(w, http.StatusBadRequest, "the request body is not valid JSON")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": req.features(nil).PromptTokens})
}

// passthrough sends any other /v1 request to llama-swap. These requests
// name a local model, so they never leave the host.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	c := clientOf(r)
	u, err := url.Parse(s.LocalURL)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			status = resp.StatusCode
			resp.Header.Set("X-Sibyl-Route", "local")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status = http.StatusBadGateway
			writeOpenAIError(w, status, err.Error())
		},
	}
	p.ServeHTTP(w, r)
	s.record(Entry{Client: c.Name, Endpoint: strings.TrimPrefix(r.URL.Path, "/v1/"), Route: "local", Status: status}, start)
}

// models lists, for the calling client, the local models, the provider
// models that its policy allows, and the routing profiles.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	c := clientOf(r)
	cfg := s.config()
	data := []json.RawMessage{}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.LocalURL+"/v1/models", nil)
	if err == nil {
		if resp, err := s.HTTP.Do(req); err == nil {
			var local struct {
				Data []json.RawMessage `json:"data"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&local) == nil {
				data = append(data, local.Data...)
			}
			_ = resp.Body.Close()
		} else {
			s.logger().Warn("list local models", "err", err)
		}
	}
	add := func(id, owner string) {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "model", "owned_by": owner, "created": 0})
		data = append(data, b)
	}
	names := make([]string, 0, len(cfg.Profiles.Profiles))
	for n := range cfg.Profiles.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == "auto" {
			add("auto", "sibyl-router")
		} else {
			add("auto:"+n, "sibyl-router")
		}
	}
	if !staysLocal(c, hint(r.Header, c)) {
		for _, p := range cfg.ProviderNames() {
			for _, m := range cfg.Providers.Providers[p].Models {
				add(p+"/"+m, p)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ExplainRequest is the body of POST /sibyl/explain.
type ExplainRequest struct {
	// Client is the client name whose policy applies.
	Client string `json:"client"`
	// Format is "chat" (the default) or "messages".
	Format string `json:"format,omitempty"`
	// Hint is the X-Sibyl-Route header to test with.
	Hint    string          `json:"hint,omitempty"`
	Request json.RawMessage `json:"request"`
}

// explain returns the decision for a request without sending it. Only the
// System API key can call it.
func (s *Server) explain(w http.ResponseWriter, r *http.Request) {
	if !equal(clientKey(r), s.AdminKey) {
		writeOpenAIError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeOpenAIError(w, errCode(err), err.Error())
		return
	}
	var er ExplainRequest
	if err := json.Unmarshal(body, &er); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "the body is not valid JSON")
		return
	}
	c, ok := s.clientByName(er.Client)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("no client %q", er.Client))
		return
	}
	var model string
	var f Features
	switch er.Format {
	case "", "chat":
		req, err := parseChat(er.Request)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error())
			return
		}
		model, f = req.Model, req.features(s.config().Profiles.SensitiveWords)
	case "messages":
		req, err := parseMessages(er.Request)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error())
			return
		}
		model, f = req.Model, req.features(s.config().Profiles.SensitiveWords)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "format must be chat or messages")
		return
	}
	h := http.Header{}
	h.Set("X-Sibyl-Route", er.Hint)
	d, err := s.resolver(c, h).decide(model, f)
	if err != nil {
		writeOpenAIError(w, errCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// clientByName finds a client by name, including the built-in clients.
func (s *Server) clientByName(name string) (Client, bool) {
	for _, c := range s.config().Clients.Clients {
		if c.Name == name {
			return c, true
		}
	}
	if Builtin(name) {
		return Client{Name: name, Policy: LocalOnly}, true
	}
	return Client{}, false
}

// Healthcheck asks a running router for /health. The container image has
// no shell or curl, so the binary checks itself.
func Healthcheck(addr string) error {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}
