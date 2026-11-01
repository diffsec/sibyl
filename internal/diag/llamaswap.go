package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LlamaSwap calls llama-swap's management API (v266). llama-swap publishes
// it on 127.0.0.1:18080 on the host only (llama-swap.container). The routes
// are /health, /running, /logs, /api/version, /api/metrics/activity, and
// /upstream/<model>/{health,metrics}, which llama-swap passes to llama-server.
type LlamaSwap struct {
	Base   string
	Client *http.Client
}

// NewLlamaSwap returns a client with a timeout per request.
func NewLlamaSwap(base string, timeout time.Duration) *LlamaSwap {
	return &LlamaSwap{Base: strings.TrimRight(base, "/"), Client: &http.Client{Timeout: timeout}}
}

// maxBody bounds every response that sibyl-diag reads.
const maxBody = 32 << 20

func (l *LlamaSwap) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return b, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return b, nil
}

// Health checks llama-swap itself.
func (l *LlamaSwap) Health(ctx context.Context) error {
	b, err := l.get(ctx, "/health")
	if err == nil && strings.TrimSpace(string(b)) != "OK" {
		err = fmt.Errorf("GET /health: unexpected body %q", truncate(string(b), 80))
	}
	return err
}

// RunningModel is one model that llama-swap started.
type RunningModel struct {
	Model string `json:"model"`
	State string `json:"state"`
}

// Running lists the started models with their state: ready, starting and so on.
func (l *LlamaSwap) Running(ctx context.Context) ([]RunningModel, error) {
	b, err := l.get(ctx, "/running")
	if err != nil {
		return nil, err
	}
	var v struct {
		Running []RunningModel `json:"running"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("GET /running: %w", err)
	}
	return v.Running, nil
}

// UpstreamHealth checks one llama-server through llama-swap.
func (l *LlamaSwap) UpstreamHealth(ctx context.Context, model string) error {
	_, err := l.get(ctx, "/upstream/"+url.PathEscape(model)+"/health")
	return err
}

// UpstreamMetrics reads one llama-server's Prometheus metrics. The Sibyl
// generator starts llama-server with --metrics.
func (l *LlamaSwap) UpstreamMetrics(ctx context.Context, model string) (Metrics, error) {
	b, err := l.get(ctx, "/upstream/"+url.PathEscape(model)+"/metrics")
	if err != nil {
		return nil, err
	}
	return ParseMetrics(string(b)), nil
}

// Activity is one request row from llama-swap.
type Activity struct {
	ID         int       `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Model      string    `json:"model"`
	ReqPath    string    `json:"req_path"`
	StatusCode int       `json:"resp_status_code"`
	Tokens     struct {
		Cached          int     `json:"cache_tokens"`
		Input           int     `json:"input_tokens"`
		Output          int     `json:"output_tokens"`
		PromptPerSecond float64 `json:"prompt_per_second"`
		TokensPerSecond float64 `json:"tokens_per_second"`
	} `json:"tokens"`
	DurationMS int    `json:"duration_ms"`
	Error      string `json:"error_msg"`
}

// Activities returns request rows sorted by id ("asc" or "desc"), from
// minID upwards. llama-swap allows 1 to 999 rows per page.
func (l *LlamaSwap) Activities(ctx context.Context, minID, limit int, order string) ([]Activity, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "sort": {"id"}, "order": {order}}
	if minID > 0 {
		q.Set("min_id", strconv.Itoa(minID))
	}
	b, err := l.get(ctx, "/api/metrics/activity?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var v struct {
		Data []Activity `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("GET /api/metrics/activity: %w", err)
	}
	return v.Data, nil
}

// Raw returns any route's body, for incident capture.
func (l *LlamaSwap) Raw(ctx context.Context, path string) ([]byte, error) { return l.get(ctx, path) }

// Metrics maps a series to its value. The key is the name, plus
// "{a=1,b=2}" with sorted labels when the series has labels.
type Metrics map[string]float64

var (
	promLine  = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)`)
	promLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// ParseMetrics reads the Prometheus text format that llama-server writes.
func ParseMetrics(text string) Metrics {
	out := Metrics{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := promLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		key := m[1]
		if m[2] != "" {
			var labels []string
			for _, l := range promLabel.FindAllStringSubmatch(m[2], -1) {
				labels = append(labels, l[1]+"="+l[2])
			}
			sort.Strings(labels)
			key += "{" + strings.Join(labels, ",") + "}"
		}
		out[key] = v
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
