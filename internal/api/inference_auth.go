package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// RouterAuth checks inference keys with the router, for the Ollama shim:
// a key is good when the router lists models for it. Good keys are kept
// for a minute, so a chat does not cost an extra call per request.
type RouterAuth struct {
	URL  string // http://router:8080
	HTTP *http.Client
	mu   sync.Mutex
	good map[[32]byte]time.Time
}

const authTTL = time.Minute

// Authorize returns nil when the router accepts the Authorization header.
func (a *RouterAuth) Authorize(ctx context.Context, auth string) error {
	if auth == "" {
		return errors.New("missing API key: send a Sibyl client key as a bearer token")
	}
	sum := sha256.Sum256([]byte(auth))
	a.mu.Lock()
	if t, ok := a.good[sum]; ok && time.Since(t) < authTTL {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	c := a.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("check the key with the router: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return errors.New("missing or invalid API key")
	}
	a.mu.Lock()
	if a.good == nil {
		a.good = map[[32]byte]time.Time{}
	}
	for k, t := range a.good {
		if time.Since(t) >= authTTL {
			delete(a.good, k)
		}
	}
	a.good[sum] = time.Now()
	a.mu.Unlock()
	return nil
}
