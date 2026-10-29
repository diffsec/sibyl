// Package owui sets Open WebUI's model connection and document embedding
// model through its admin API, so Open WebUI follows the Sibyl configuration.
package owui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// statusError is a non-200 response from Open WebUI.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	var detail struct {
		Detail string `json:"detail"`
	}
	msg := e.body
	if json.Unmarshal([]byte(e.body), &detail) == nil && detail.Detail != "" {
		msg = detail.Detail
	}
	if e.code == http.StatusUnauthorized || e.code == http.StatusForbidden {
		return fmt.Sprintf("open webui refused the request (%d): %s", e.code, msg)
	}
	return fmt.Sprintf("open webui %d: %s", e.code, msg)
}

// Embedding is Open WebUI's embedding configuration.
type Embedding struct {
	Engine string
	Model  string
	URL    string
	Key    string
}

// Client calls Open WebUI with an admin API key.
type Client struct {
	Base string // http://open-webui:8080
	Key  string
	HTTP *http.Client
}

// New returns a client with a request timeout.
func New(base, key string) *Client {
	return &Client{Base: strings.TrimSuffix(base, "/"), Key: key, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.call(ctx, c.Key, method, path, in, out)
}

// call sends one request with bearer as the credential: an API key or a
// session token from sign-in.
func (c *Client) call(ctx context.Context, bearer, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(raw))}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Embedding reads the current embedding configuration. It needs an admin key.
func (c *Client) Embedding(ctx context.Context) (Embedding, error) {
	var r struct {
		Engine       string `json:"RAG_EMBEDDING_ENGINE"`
		Model        string `json:"RAG_EMBEDDING_MODEL"`
		OpenAIConfig struct {
			URL string `json:"url"`
			Key string `json:"key"`
		} `json:"openai_config"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/retrieval/embedding", nil, &r); err != nil {
		return Embedding{}, err
	}
	return Embedding{Engine: r.Engine, Model: r.Model, URL: r.OpenAIConfig.URL, Key: r.OpenAIConfig.Key}, nil
}

// SetEmbedding points Open WebUI at an OpenAI-compatible embedding model.
func (c *Client) SetEmbedding(ctx context.Context, model, url, key string) error {
	body := map[string]any{
		"RAG_EMBEDDING_ENGINE":     "openai",
		"RAG_EMBEDDING_MODEL":      model,
		"RAG_EMBEDDING_BATCH_SIZE": 8,
		"openai_config":            map[string]string{"url": url, "key": key},
	}
	return c.do(ctx, http.MethodPost, "/api/v1/retrieval/embedding/update", body, nil)
}

// Sync sets the embedding model if Open WebUI uses a different one. It
// returns true when it changed the configuration.
func (c *Client) Sync(ctx context.Context, model, url, key string) (bool, error) {
	cur, err := c.Embedding(ctx)
	if err != nil {
		return false, err
	}
	if cur.Engine == "openai" && cur.Model == model && cur.URL == url && cur.Key == key {
		return false, nil
	}
	return true, c.SetEmbedding(ctx, model, url, key)
}

// connections is Open WebUI's list of OpenAI-compatible connections.
type connections struct {
	Enable  *bool          `json:"ENABLE_OPENAI_API"`
	URLs    []string       `json:"OPENAI_API_BASE_URLS"`
	Keys    []string       `json:"OPENAI_API_KEYS"`
	Configs map[string]any `json:"OPENAI_API_CONFIGS"`
}

// SyncConnection makes Open WebUI reach its models at url with key. It
// replaces the connection whose URL is url or one of old, and adds one if
// none matches. Connections that the admin added are kept. It returns true
// when it changed the configuration.
func (c *Client) SyncConnection(ctx context.Context, url, key string, old ...string) (bool, error) {
	var cur connections
	if err := c.do(ctx, http.MethodGet, "/openai/config", nil, &cur); err != nil {
		return false, err
	}
	for len(cur.Keys) < len(cur.URLs) {
		cur.Keys = append(cur.Keys, "")
	}
	if cur.Configs == nil {
		cur.Configs = map[string]any{}
	}
	enabled := cur.Enable != nil && *cur.Enable
	i := -1
	for j, u := range cur.URLs {
		if strings.TrimSuffix(u, "/") == url || slices.Contains(old, strings.TrimSuffix(u, "/")) {
			i = j
			break
		}
	}
	if i >= 0 && enabled && cur.URLs[i] == url && cur.Keys[i] == key {
		return false, nil
	}
	if i < 0 {
		cur.URLs = append(cur.URLs, url)
		cur.Keys = append(cur.Keys, key)
	} else {
		cur.URLs[i], cur.Keys[i] = url, key
	}
	on := true
	cur.Enable = &on
	return true, c.do(ctx, http.MethodPost, "/openai/config/update", cur, nil)
}

// ErrAdminExists means sign-up was refused because Open WebUI already has users.
var ErrAdminExists = errors.New("open webui already has an admin: sign in with that account instead")

type session struct {
	Token string `json:"token"`
	Role  string `json:"role"`
}

// SignIn logs in with a password and returns a session token. The token must
// belong to an admin.
func (c *Client) SignIn(ctx context.Context, email, password string) (string, error) {
	var s session
	err := c.call(ctx, "", http.MethodPost, "/api/v1/auths/signin", map[string]string{"email": email, "password": password}, &s)
	if err != nil {
		return "", fmt.Errorf("sign in: %w", err)
	}
	return checkAdmin(s)
}

// SignUp creates the first account, which Open WebUI makes the admin.
func (c *Client) SignUp(ctx context.Context, name, email, password string) (string, error) {
	var s session
	err := c.call(ctx, "", http.MethodPost, "/api/v1/auths/signup",
		map[string]string{"name": name, "email": email, "password": password}, &s)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusForbidden {
		return "", ErrAdminExists
	}
	if err != nil {
		return "", fmt.Errorf("sign up: %w", err)
	}
	return checkAdmin(s)
}

func checkAdmin(s session) (string, error) {
	if s.Token == "" {
		return "", errors.New("open webui returned no session token")
	}
	if s.Role != "admin" {
		return "", fmt.Errorf("this account is a %q, not an admin", s.Role)
	}
	return s.Token, nil
}

// EnableAPIKeys turns on API keys in the admin config, if they are off. It
// sends back every other admin setting unchanged.
func (c *Client) EnableAPIKeys(ctx context.Context, token string) (bool, error) {
	cfg := map[string]any{}
	if err := c.call(ctx, token, http.MethodGet, "/api/v1/auths/admin/config", nil, &cfg); err != nil {
		return false, err
	}
	if on, _ := cfg["ENABLE_API_KEYS"].(bool); on {
		return false, nil
	}
	cfg["ENABLE_API_KEYS"] = true
	return true, c.call(ctx, token, http.MethodPost, "/api/v1/auths/admin/config", cfg, nil)
}

// APIKey returns the account's API key, and creates one only if it has none.
// Creating a key replaces the old one, so an existing key is reused.
func (c *Client) APIKey(ctx context.Context, token string) (string, error) {
	var k struct {
		Key string `json:"api_key"`
	}
	err := c.call(ctx, token, http.MethodGet, "/api/v1/auths/api_key", nil, &k)
	var se *statusError
	switch {
	case err == nil && k.Key != "":
		return k.Key, nil
	case err == nil || (errors.As(err, &se) && se.code == http.StatusNotFound):
	default:
		return "", err
	}
	if err := c.call(ctx, token, http.MethodPost, "/api/v1/auths/api_key", nil, &k); err != nil {
		return "", err
	}
	if k.Key == "" {
		return "", errors.New("open webui returned an empty API key")
	}
	return k.Key, nil
}
