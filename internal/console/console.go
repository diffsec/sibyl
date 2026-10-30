// Package console is the Sibyl web console: a browser view of the TUI.
// docs/webui-design.md describes it.
//
// The console signs the owner in with the password of a wheel account,
// which sibyl-hostd checks, and keeps a session in a cookie. Every page
// calls the System API through the same client as the TUI, in process, so
// the console has no power that the API does not give the TUI.
//
// static/htmx-2.0.4.min.js is htmx 2.0.4 from the npm registry, checked
// against its integrity hash sha512-HLxMCdfXDOJirs3vBZl/ZLoY+c7PfM4Ahr2Ad4YXh6d22T5ltbTXFFkpx9Tgb2vvmWFMbIc3LqN2ToNkZJvyYQ==.
package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/client"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// The session limits.
const (
	SessionMax  = 12 * time.Hour
	SessionIdle = time.Hour
	// CookieName is the session cookie.
	CookieName = "sibyl_console"
	// Prefix is the path of every console page.
	Prefix = "/console/"
	// failLimit is the number of sign-ins per address per minute that can
	// fail. sibyl-hostd adds its own limit per account.
	failLimit = 5
	// maxChecks is the number of password checks that run at once.
	maxChecks = 2
)

// CSP is the Content Security Policy of every console response. No page
// has an inline script or style.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// ErrDenied is the error that Check returns for a wrong user or password.
var ErrDenied = errors.New("wrong user or password")

// Console serves the web console under Prefix.
type Console struct {
	// API is the System API handler. The console calls it in process.
	API http.Handler
	// Token is the System API key that the console adds to each call.
	Token string
	// Check checks a user and password. It returns ErrDenied, or an error
	// that wraps it, for a wrong user or password.
	Check func(ctx context.Context, user, password string) error
	Log   *slog.Logger
	Now   func() time.Time
	// TrustProxy reports whether a request from this address comes from
	// Caddy, whose X-Forwarded-For names the browser. Nil trusts none.
	TrustProxy func(ip string) bool

	once     sync.Once
	client   *client.Client
	pages    map[string]*template.Template
	mu       sync.Mutex
	sessions map[[32]byte]*session
	fails    map[string][]time.Time
	checks   chan struct{}
}

type session struct {
	user          string
	created, seen time.Time
}

func (c *Console) init() {
	c.once.Do(func() {
		if c.Now == nil {
			c.Now = time.Now
		}
		if c.Log == nil {
			c.Log = slog.Default()
		}
		c.sessions = map[[32]byte]*session{}
		c.fails = map[string][]time.Time{}
		c.checks = make(chan struct{}, maxChecks)
		c.client = &client.Client{Base: "http://system-api", Token: c.Token,
			HTTP: &http.Client{Timeout: 60 * time.Second, Transport: inProcess{c.API}}}
		c.pages = parsePages()
	})
}

// Handler serves every path under Prefix.
func (c *Console) Handler() http.Handler {
	c.init()
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET "+Prefix+"static/", http.StripPrefix(Prefix+"static/", http.FileServerFS(static)))
	mux.HandleFunc("GET "+Prefix+"login", c.loginPage)
	mux.HandleFunc("POST "+Prefix+"login", c.login)
	mux.HandleFunc("POST "+Prefix+"logout", c.signedIn(c.logout))
	mux.HandleFunc("GET "+Prefix+"{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"router", http.StatusSeeOther)
	})
	c.routes(mux)
	return headers(mux)
}

// headers sets the security headers of every console response.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, Prefix+"static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether a request that changes something came from a
// console page. A browser sends Origin with every POST.
func sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://"+r.Host
}

func hashID(id string) [32]byte { return sha256.Sum256([]byte(id)) }

// user returns the signed-in user of a request. A request that the owner
// made refreshes the session, but an htmx poll does not, so an open page
// still ends after SessionIdle.
func (c *Console) user(r *http.Request) (string, bool) {
	ck, err := r.Cookie(CookieName)
	if err != nil || ck.Value == "" {
		return "", false
	}
	now := c.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	key := hashID(ck.Value)
	s, ok := c.sessions[key]
	if !ok {
		return "", false
	}
	if now.Sub(s.created) > SessionMax || now.Sub(s.seen) > SessionIdle {
		delete(c.sessions, key)
		return "", false
	}
	if r.Header.Get("HX-Request") != "true" {
		s.seen = now
	}
	return s.user, true
}

type ctxUser struct{}

// signedIn sends a request with no session to the sign-in page, and
// refuses a request that changes something from another origin.
func (c *Console) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := c.user(r)
		if !ok {
			if r.Header.Get("HX-Request") == "true" {
				// htmx follows HX-Redirect with a full page load.
				w.Header().Set("HX-Redirect", Prefix+"login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, Prefix+"login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "this request did not come from a console page", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
	}
}

func userOf(r *http.Request) string { u, _ := r.Context().Value(ctxUser{}).(string); return u }

// clientAddr is the browser's address. Caddy sets X-Forwarded-For and
// replaces a value that the browser sends. The header counts only from
// Caddy, because another container could send any value.
func (c *Console) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if f := r.Header.Get("X-Forwarded-For"); f != "" && c.TrustProxy != nil && c.TrustProxy(host) {
		first, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(first)
	}
	return host
}

// attempt counts a sign-in against addr before its check runs, so that
// parallel requests cannot pass the limit. It returns false when addr
// has used its tries for the minute. A sign-in that passes takes its try
// back.
func (c *Console) attempt(addr string) bool {
	now := c.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for a, ts := range c.fails {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Minute {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(c.fails, a)
			continue
		}
		c.fails[a] = kept
	}
	if len(c.fails[addr]) >= failLimit {
		return false
	}
	c.fails[addr] = append(c.fails[addr], now)
	return true
}

// passed takes back the try of a sign-in that passed.
func (c *Console) passed(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ts := c.fails[addr]; len(ts) > 0 {
		c.fails[addr] = ts[:len(ts)-1]
	}
}

func (c *Console) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.user(r); ok {
		http.Redirect(w, r, Prefix, http.StatusSeeOther)
		return
	}
	c.render(w, r, http.StatusOK, "login", map[string]any{})
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	addr := c.clientAddr(r)
	if !sameOrigin(r) {
		http.Error(w, "this request did not come from a console page", http.StatusForbidden)
		return
	}
	fail := func(code int, msg string) {
		c.render(w, r, code, "login", map[string]any{"Error": msg, "Name": r.PostFormValue("user")})
	}
	if !c.attempt(addr) {
		c.Log.Warn("console sign-in refused: too many tries", "addr", addr)
		fail(http.StatusTooManyRequests, "Too many sign-ins. Wait a minute.")
		return
	}
	select {
	case c.checks <- struct{}{}:
		defer func() { <-c.checks }()
	default:
		fail(http.StatusTooManyRequests, "Other sign-ins are being checked. Try again.")
		return
	}
	user := strings.TrimSpace(r.PostFormValue("user"))
	password := r.PostFormValue("password")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := c.Check(ctx, user, password)
	switch {
	case errors.Is(err, ErrDenied):
		c.Log.Warn("console sign-in failed", "user", user, "addr", addr)
		fail(http.StatusUnauthorized, "Wrong user or password. The user must be in the wheel group.")
		return
	case err != nil:
		c.Log.Error("console sign-in check", "err", err)
		fail(http.StatusServiceUnavailable, "The host did not check the password. Try again.")
		return
	}
	c.passed(addr)
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		fail(http.StatusInternalServerError, "No session could be made.")
		return
	}
	id := base64.RawURLEncoding.EncodeToString(b[:])
	now := c.Now()
	c.mu.Lock()
	c.sessions[hashID(id)] = &session{user: user, created: now, seen: now}
	c.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: id, Path: Prefix, MaxAge: int(SessionMax.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	c.Log.Info("console sign-in", "user", user, "addr", addr)
	http.Redirect(w, r, Prefix, http.StatusSeeOther)
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(CookieName); err == nil {
		c.mu.Lock()
		delete(c.sessions, hashID(ck.Value))
		c.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: Prefix, MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	c.Log.Info("console sign-out", "user", userOf(r))
	http.Redirect(w, r, Prefix+"login", http.StatusSeeOther)
}
