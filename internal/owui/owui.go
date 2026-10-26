// Package owui sets Open WebUI's document embedding model through its admin
// API, so it follows the embedding model installed in Sibyl.
package owui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedding is Open WebUI's embedding configuration.
type Embedding struct {
	Engine string
	Model  string
	URL    string
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
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("open webui refused the key (%d): it must be an admin's API key, and API keys must be enabled in Admin Panel, Settings, General", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("open webui %s: %s", resp.Status, strings.TrimSpace(string(raw)))
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
		} `json:"openai_config"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/retrieval/embedding", nil, &r); err != nil {
		return Embedding{}, err
	}
	return Embedding{Engine: r.Engine, Model: r.Model, URL: r.OpenAIConfig.URL}, nil
}

// SetEmbedding points Open WebUI at an OpenAI-compatible embedding model.
func (c *Client) SetEmbedding(ctx context.Context, model, url string) error {
	body := map[string]any{
		"RAG_EMBEDDING_ENGINE":     "openai",
		"RAG_EMBEDDING_MODEL":      model,
		"RAG_EMBEDDING_BATCH_SIZE": 8,
		"openai_config":            map[string]string{"url": url, "key": "none"},
	}
	return c.do(ctx, http.MethodPost, "/api/v1/retrieval/embedding/update", body, nil)
}

// Sync sets the embedding model if Open WebUI uses a different one. It
// returns true when it changed the configuration.
func (c *Client) Sync(ctx context.Context, model, url string) (bool, error) {
	cur, err := c.Embedding(ctx)
	if err != nil {
		return false, err
	}
	if cur.Engine == "openai" && cur.Model == model && cur.URL == url {
		return false, nil
	}
	return true, c.SetEmbedding(ctx, model, url)
}
