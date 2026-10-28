// Package client is a typed client for the Sibyl System API.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/disks"
	"github.com/diffsec/sibyl/internal/download"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/hf"
	"github.com/diffsec/sibyl/internal/hostctl"
	"github.com/diffsec/sibyl/internal/hostd"
	"github.com/diffsec/sibyl/internal/llamaswap"
	"github.com/diffsec/sibyl/internal/models"
)

// Client calls the System API at Base with a bearer token.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// New returns a client. caFile, if set, is a PEM file of extra trusted CA
// certificates, such as the host's self-signed /var/lib/sibyl/tls/server.crt.
// serverName, if set, is the name to verify instead of the URL's host, for
// connecting to 127.0.0.1 with a certificate issued for sibyl.local.
func New(base, token, caFile, serverName string) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificates found", caFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &Client{Base: strings.TrimSuffix(base, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second, Transport: tr}}, nil
}

// APIError is an error response from the API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

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
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Status is the GET /status response.
type Status struct {
	Version  string             `json:"version"`
	ImageTag string             `json:"image_tag"`
	GPUs     int                `json:"gpus"`
	Variant  hardware.Variant   `json:"variant"`
	Backends []hardware.Backend `json:"backends"`
	Hardware hardware.Config    `json:"hardware"`
	Services hostctl.Status     `json:"services"`
	Errors   []string           `json:"errors,omitempty"`
}

// Model is one entry of GET /models.
type Model struct {
	models.Model
	Settings  models.Settings `json:"settings"`
	Benchmark *Benchmark      `json:"benchmark,omitempty"`
	Meta      *models.Meta    `json:"meta,omitempty"`
}

// BackendResult is one backend's llama-bench result.
type BackendResult struct {
	PromptTPS float64 `json:"prompt_tokens_per_second,omitempty"`
	GenTPS    float64 `json:"generation_tokens_per_second,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Benchmark is a model's benchmark of the variant's backends.
type Benchmark struct {
	Status      string                   `json:"status"`
	Updated     string                   `json:"updated,omitempty"`
	Results     map[string]BackendResult `json:"results,omitempty"`
	Recommended string                   `json:"recommended,omitempty"`
}

// ApplyResult is the response of every call that regenerates the config.
type ApplyResult struct {
	Models         int               `json:"models"`
	Issues         []llamaswap.Issue `json:"issues,omitempty"`
	ConfigChanged  bool              `json:"config_changed"`
	BackendChanged bool              `json:"backend_changed"`
}

// Variant is one downloadable variant with its fit rating.
type Variant struct {
	hf.Variant
	Fit string `json:"fit"`
}

// Repo is the GET /hf/repos/{owner}/{name} response.
type Repo struct {
	Repo     string    `json:"repo"`
	Revision string    `json:"revision"`
	VRAM     int64     `json:"vram_bytes"`
	RAM      int64     `json:"ram_bytes"`
	Variants []Variant `json:"variants"`
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/status", nil, &s)
}

func (c *Client) GPUs(ctx context.Context) ([]gpu.GPU, error) {
	var g []gpu.GPU
	return g, c.do(ctx, http.MethodGet, "/gpus", nil, &g)
}

func (c *Client) Hardware(ctx context.Context) (hardware.Config, error) {
	var h hardware.Config
	return h, c.do(ctx, http.MethodGet, "/hardware", nil, &h)
}

func (c *Client) SetHardware(ctx context.Context, h hardware.Config) (ApplyResult, error) {
	var r ApplyResult
	return r, c.do(ctx, http.MethodPut, "/hardware", h, &r)
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var m []Model
	return m, c.do(ctx, http.MethodGet, "/models", nil, &m)
}

func (c *Client) SetModel(ctx context.Context, id string, s models.Settings) (ApplyResult, error) {
	var r ApplyResult
	return r, c.do(ctx, http.MethodPut, "/models/"+url.PathEscape(id), s, &r)
}

func (c *Client) DeleteModel(ctx context.Context, id string) (ApplyResult, error) {
	var r ApplyResult
	return r, c.do(ctx, http.MethodDelete, "/models/"+url.PathEscape(id), nil, &r)
}

func (c *Client) StartBenchmark(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/models/"+url.PathEscape(id)+"/benchmark", nil, nil)
}

// OpenWebUI is the Open WebUI integration status.
type OpenWebUI struct {
	Configured bool   `json:"configured"`
	Desired    string `json:"embedding_model,omitempty"`
	LastSync   string `json:"last_sync,omitempty"`
	LastSyncAt string `json:"last_sync_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (c *Client) OpenWebUI(ctx context.Context) (OpenWebUI, error) {
	var o OpenWebUI
	return o, c.do(ctx, http.MethodGet, "/integrations/open-webui", nil, &o)
}

func (c *Client) SetOpenWebUIKey(ctx context.Context, key string) (OpenWebUI, error) {
	var o OpenWebUI
	return o, c.do(ctx, http.MethodPut, "/integrations/open-webui", map[string]string{"api_key": key}, &o)
}

// ConnectOpenWebUI logs in to Open WebUI as an admin, or creates the first
// admin, and lets the System API store an API key. The password is not stored.
func (c *Client) ConnectOpenWebUI(ctx context.Context, createAdmin bool, name, email, password string) (OpenWebUI, error) {
	var o OpenWebUI
	in := map[string]any{"create_admin": createAdmin, "name": name, "email": email, "password": password}
	return o, c.do(ctx, http.MethodPost, "/integrations/open-webui/connect", in, &o)
}

func (c *Client) Restart(ctx context.Context, unit string) error {
	return c.do(ctx, http.MethodPost, "/services/"+url.PathEscape(unit)+"/restart", nil, nil)
}

func (c *Client) Search(ctx context.Context, q string) ([]hf.Summary, error) {
	var s []hf.Summary
	return s, c.do(ctx, http.MethodGet, "/hf/search?"+url.Values{"q": {q}, "limit": {"30"}}.Encode(), nil, &s)
}

func (c *Client) Repo(ctx context.Context, id string) (Repo, error) {
	var r Repo
	owner, name, ok := strings.Cut(id, "/")
	if !ok {
		return r, fmt.Errorf("repository %q is not owner/name", id)
	}
	return r, c.do(ctx, http.MethodGet, "/hf/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &r)
}

// Download queues a download. pool is empty for the models folder.
func (c *Client) Download(ctx context.Context, repo, revision, variant, pool string) (download.Job, error) {
	var j download.Job
	in := map[string]string{"repo": repo, "revision": revision, "variant": variant, "pool": pool}
	return j, c.do(ctx, http.MethodPost, "/downloads", in, &j)
}

func (c *Client) Downloads(ctx context.Context) ([]download.Job, error) {
	var j []download.Job
	return j, c.do(ctx, http.MethodGet, "/downloads", nil, &j)
}

func (c *Client) CancelDownload(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/downloads/"+url.PathEscape(id), nil, nil)
}

func (c *Client) RetryDownload(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/downloads/"+url.PathEscape(id)+"/retry", nil, nil)
}

// Host reads the hostname, network interfaces and accounts.
func (c *Client) Host(ctx context.Context) (hostd.State, error) {
	var s hostd.State
	return s, c.do(ctx, http.MethodGet, "/host", nil, &s)
}

// A host action returns its result. A result with an empty Finished is still
// queued: the host agent did not answer in time.
func (c *Client) hostAction(ctx context.Context, method, path string, in any) (hostd.Result, error) {
	var r hostd.Result
	return r, c.do(ctx, method, path, in, &r)
}

func (c *Client) SetHostname(ctx context.Context, name string) (hostd.Result, error) {
	return c.hostAction(ctx, http.MethodPut, "/host/hostname", hostd.Hostname{Hostname: name})
}

// SetNetwork changes one interface. The change rolls back unless
// ConfirmNetwork follows within n.RollbackSeconds.
func (c *Client) SetNetwork(ctx context.Context, n hostd.Network) (hostd.Result, error) {
	iface := n.Interface
	n.Interface = ""
	return c.hostAction(ctx, http.MethodPut, "/host/network/"+url.PathEscape(iface), n)
}

func (c *Client) ConfirmNetwork(ctx context.Context) (hostd.Result, error) {
	return c.hostAction(ctx, http.MethodPost, "/host/network/confirm", nil)
}

// SetUser creates an operator account or replaces its keys. github, if set,
// adds the keys that GitHub publishes for that account.
func (c *Client) SetUser(ctx context.Context, name string, keys []string, github string) (hostd.Result, error) {
	if keys == nil {
		keys = []string{}
	}
	in := map[string]any{"keys": keys}
	if github != "" {
		in["github"] = github
	}
	return c.hostAction(ctx, http.MethodPut, "/host/users/"+url.PathEscape(name), in)
}

func (c *Client) DeleteUser(ctx context.Context, name string) (hostd.Result, error) {
	return c.hostAction(ctx, http.MethodDelete, "/host/users/"+url.PathEscape(name), nil)
}

// Pools is the GET /pools response.
type Pools struct {
	Pools []hostd.Pool `json:"pools"`
	Disks []disks.Disk `json:"disks"`
}

func (c *Client) Pools(ctx context.Context) (Pools, error) {
	var p Pools
	return p, c.do(ctx, http.MethodGet, "/pools", nil, &p)
}

// CreatePool erases a disk and makes it a model pool. confirm is the disk's
// serial number, or "erase" when it has none. The result's Secret is the
// pool's recovery key.
func (c *Client) CreatePool(ctx context.Context, disk, confirm string) (hostd.Result, error) {
	return c.hostAction(ctx, http.MethodPost, "/pools", hostd.CreatePoolReq{Disk: disk, Confirm: confirm})
}

func (c *Client) DetachPool(ctx context.Context, name string) (hostd.Result, error) {
	return c.hostAction(ctx, http.MethodDelete, "/pools/"+url.PathEscape(name), nil)
}

// HuggingFace is the GET /integrations/huggingface response.
type HuggingFace struct {
	Configured bool   `json:"configured"`
	User       string `json:"user,omitempty"`
	SetAt      string `json:"set_at,omitempty"`
}

func (c *Client) HuggingFace(ctx context.Context) (HuggingFace, error) {
	var h HuggingFace
	return h, c.do(ctx, http.MethodGet, "/integrations/huggingface", nil, &h)
}

// SetHuggingFaceToken stores an access token after the System API checks it
// with Hugging Face.
func (c *Client) SetHuggingFaceToken(ctx context.Context, token string) (HuggingFace, error) {
	var h HuggingFace
	return h, c.do(ctx, http.MethodPut, "/integrations/huggingface", map[string]string{"token": token}, &h)
}

func (c *Client) ClearHuggingFaceToken(ctx context.Context) (HuggingFace, error) {
	var h HuggingFace
	return h, c.do(ctx, http.MethodDelete, "/integrations/huggingface", nil, &h)
}
