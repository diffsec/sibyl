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
	"github.com/diffsec/sibyl/internal/router"
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
	// Fit is what the System API chose for the context and the split.
	Fit *models.Fit `json:"fit,omitempty"`
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

// Arcane is the GET /integrations/arcane response.
type Arcane struct {
	User string `json:"user"`
	// Password is the password that Sibyl set, until the owner changes it.
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Arcane returns the Arcane login that Sibyl set, if the owner has not
// changed it yet.
func (c *Client) Arcane(ctx context.Context) (Arcane, error) {
	var a Arcane
	return a, c.do(ctx, http.MethodGet, "/integrations/arcane", nil, &a)
}

// RouterBudget limits a client's provider spend, in US dollars per UTC day
// and month. Zero means no limit.
type RouterBudget struct {
	DailyUSD   float64 `json:"daily_usd,omitempty"`
	MonthlyUSD float64 `json:"monthly_usd,omitempty"`
}

// RouterClient is one inference client of the model router.
type RouterClient struct {
	Name      string        `json:"name"`
	Policy    string        `json:"policy"`
	AllowHint bool          `json:"allow_hint"`
	Builtin   bool          `json:"builtin"`
	Budget    *RouterBudget `json:"budget,omitempty"`
	Calibrate bool          `json:"calibrate,omitempty"`
}

// RouterProvider is one provider of the model router.
type RouterProvider struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url,omitempty"`
	Models  []string `json:"models,omitempty"`
	KeySet  bool     `json:"key_set"`
	// Prices are the owner's prices by model name, and Catalog the
	// provider's models.dev ID.
	Prices  map[string]router.Price `json:"prices,omitempty"`
	Catalog string                  `json:"catalog,omitempty"`
}

// Router is the GET /router response. Profiles is the whole profiles
// file, so a change from the TUI keeps every setting it does not touch.
type Router struct {
	Clients   []RouterClient      `json:"clients"`
	Providers []RouterProvider    `json:"providers"`
	Profiles  router.ProfilesFile `json:"profiles"`
}

// SetRouterProfiles replaces the profiles file after the router's checks.
func (c *Client) SetRouterProfiles(ctx context.Context, f router.ProfilesFile) error {
	return c.do(ctx, http.MethodPut, "/router/profiles", f, nil)
}

// ClearCalibration deletes every calibration sample and returns the count.
func (c *Client) ClearCalibration(ctx context.Context) (int, error) {
	var r struct {
		Deleted int `json:"deleted"`
	}
	return r.Deleted, c.do(ctx, http.MethodDelete, "/router/calibration", nil, &r)
}

// RouterEntry is one line of the router's decision log.
type RouterEntry struct {
	Time      time.Time `json:"time"`
	Client    string    `json:"client"`
	Requested string    `json:"requested"`
	Route     string    `json:"route"`
	Rule      string    `json:"rule"`
	Status    int       `json:"status"`
	CostUSD   float64   `json:"cost_usd"`
	Error     string    `json:"error"`
}

// RouterSpend is a client's provider spend in US dollars.
type RouterSpend struct {
	Today float64 `json:"today_usd"`
	Month float64 `json:"month_usd"`
}

// RouterStats is the GET /router/stats response.
type RouterStats struct {
	Requests int            `json:"requests"`
	ByClient map[string]int `json:"by_client"`
	ByRoute  map[string]int `json:"by_route"`
	Errors   int            `json:"errors"`
	Unpriced int            `json:"unpriced"`
	// Regenerated counts, per route, the answers that a client asked for
	// again within a minute.
	Regenerated map[string]int         `json:"regenerated"`
	Spend       map[string]RouterSpend `json:"spend"`
	Last        []RouterEntry          `json:"last"`
}

// NewRouterClient is a created client with its key, which is shown once.
type NewRouterClient struct {
	Name   string `json:"name"`
	Policy string `json:"policy"`
	Key    string `json:"key"`
}

// ProviderRequest adds or changes a provider. An empty APIKey keeps the
// stored key.
type ProviderRequest struct {
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url,omitempty"`
	APIKey  string   `json:"api_key,omitempty"`
	Models  []string `json:"models,omitempty"`
	// Catalog is the provider's models.dev ID, for prices.
	Catalog string `json:"catalog,omitempty"`
	// Prices replace the provider's prices. A change of a provider must
	// send its current prices, or they are lost.
	Prices map[string]router.Price `json:"prices,omitempty"`
}

// Router returns the router's clients, providers and profiles.
func (c *Client) Router(ctx context.Context) (Router, error) {
	var r Router
	return r, c.do(ctx, http.MethodGet, "/router", nil, &r)
}

// RouterStats returns the totals and the last decisions of the router.
func (c *Client) RouterStats(ctx context.Context) (RouterStats, error) {
	var r RouterStats
	return r, c.do(ctx, http.MethodGet, "/router/stats", nil, &r)
}

// CalibrationCell is the calibration result for one local model, task and
// difficulty.
type CalibrationCell struct {
	LocalModel        string  `json:"local_model"`
	Task              string  `json:"task"`
	Difficulty        string  `json:"difficulty"`
	Comparisons       int     `json:"comparisons"`
	LocalOKRate       float64 `json:"local_ok_rate"`
	ProviderCostUSD   float64 `json:"provider_cost_usd"`
	LocalLatencyMS    int64   `json:"local_latency_ms"`
	ProviderLatencyMS int64   `json:"provider_latency_ms"`
}

// CalibrationProposal is a rule change that the calibration supports.
type CalibrationProposal struct {
	ID          string  `json:"id"`
	Profile     string  `json:"profile"`
	Task        string  `json:"task"`
	Difficulty  string  `json:"difficulty"`
	Current     string  `json:"current"`
	Route       string  `json:"route"`
	Comparisons int     `json:"comparisons"`
	LocalOKRate float64 `json:"local_ok_rate"`
	Allowed     bool    `json:"allowed"`
}

// RouterCalibration is the calibration report.
type RouterCalibration struct {
	Samples int `json:"samples"`
	Pending int `json:"pending"`
	Judged  int `json:"judged"`
	Failed  int `json:"failed"`
	Status  *struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	} `json:"status"`
	SpentTodayUSD float64               `json:"spent_today_usd"`
	DailyUSD      float64               `json:"daily_usd"`
	AutoApply     bool                  `json:"auto_apply"`
	Cells         []CalibrationCell     `json:"cells"`
	Proposals     []CalibrationProposal `json:"proposals"`
}

// RouterCalibration returns the calibration report and the proposals.
func (c *Client) RouterCalibration(ctx context.Context) (RouterCalibration, error) {
	var r RouterCalibration
	return r, c.do(ctx, http.MethodGet, "/router/calibration", nil, &r)
}

// ApplyCalibration adds the rule of a proposal to its profile.
func (c *Client) ApplyCalibration(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/router/calibration/apply", map[string]string{"id": id}, nil)
}

// CreateRouterClient creates a client and returns its key.
func (c *Client) CreateRouterClient(ctx context.Context, name, policy string, budget *RouterBudget) (NewRouterClient, error) {
	var r NewRouterClient
	in := map[string]any{"name": name, "policy": policy, "budget": budget}
	return r, c.do(ctx, http.MethodPost, "/router/clients", in, &r)
}

// SetRouterClient sets a client's policy, budget and calibration. A nil
// budget removes it.
func (c *Client) SetRouterClient(ctx context.Context, name, policy string, allowHint bool, budget *RouterBudget, calibrate bool) error {
	in := map[string]any{"policy": policy, "allow_hint": allowHint, "budget": budget, "calibrate": calibrate}
	return c.do(ctx, http.MethodPut, "/router/clients/"+url.PathEscape(name), in, nil)
}

// NewRouterClientKey gives a client a new key. The old key stops working.
func (c *Client) NewRouterClientKey(ctx context.Context, name string) (NewRouterClient, error) {
	var r NewRouterClient
	return r, c.do(ctx, http.MethodPost, "/router/clients/"+url.PathEscape(name)+"/key", struct{}{}, &r)
}

// DeleteRouterClient deletes a client, which revokes its key.
func (c *Client) DeleteRouterClient(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/router/clients/"+url.PathEscape(name), nil, nil)
}

// SetRouterProvider adds or changes a provider. The API checks the key
// with the provider first.
func (c *Client) SetRouterProvider(ctx context.Context, name string, p ProviderRequest) error {
	return c.do(ctx, http.MethodPut, "/router/providers/"+url.PathEscape(name), p, nil)
}

// DeleteRouterProvider deletes a provider and its key.
func (c *Client) DeleteRouterProvider(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/router/providers/"+url.PathEscape(name), nil, nil)
}

// CatalogProvider is one models.dev provider that the router can use.
type CatalogProvider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url"`
	Doc     string   `json:"doc"`
	Env     []string `json:"env"`
	Models  int      `json:"models"`
}

// Catalog is the GET /router/catalog response.
type Catalog struct {
	Fetched   string            `json:"fetched"`
	Enabled   bool              `json:"enabled"`
	Error     string            `json:"error"`
	Providers []CatalogProvider `json:"providers"`
}

// CatalogModel is one model of a models.dev provider.
type CatalogModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Context  int    `json:"context"`
	ToolCall bool   `json:"tool_call"`
	Cost     *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
}

// CatalogModels is the GET /router/catalog/{id} response.
type CatalogModels struct {
	CatalogProvider
	Models []CatalogModel `json:"models"`
}

// Catalog returns the providers of the local models.dev index.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	var r Catalog
	return r, c.do(ctx, http.MethodGet, "/router/catalog", nil, &r)
}

// CatalogProvider returns one models.dev provider with its models.
func (c *Client) CatalogProvider(ctx context.Context, id string) (CatalogModels, error) {
	var r CatalogModels
	return r, c.do(ctx, http.MethodGet, "/router/catalog/"+url.PathEscape(id), nil, &r)
}
