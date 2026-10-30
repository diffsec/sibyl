package router

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Entry is one line of the decision log. It never holds message content.
type Entry struct {
	Time             time.Time `json:"time"`
	Client           string    `json:"client"`
	Endpoint         string    `json:"endpoint"`
	Requested        string    `json:"requested,omitempty"`
	Profile          string    `json:"profile,omitempty"`
	Rule             string    `json:"rule,omitempty"`
	Task             string    `json:"task,omitempty"`
	Difficulty       string    `json:"difficulty,omitempty"`
	Route            string    `json:"route,omitempty"`
	Status           int       `json:"status"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CacheReadTokens  int       `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int       `json:"cache_write_tokens,omitempty"`
	// CostUSD is the provider cost from the price table. Unpriced is true
	// for a provider route with no price, whose cost is not known.
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Unpriced  bool    `json:"unpriced,omitempty"`
	LatencyMS int64   `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
	// Regenerates is the route of an answer to the same conversation
	// within the last minute. The client asked again, so it likely did
	// not like that answer.
	Regenerates string `json:"regenerates,omitempty"`
}

// MaxLogSize is the size at which the decision log rotates. One old file,
// <path>.1, is kept.
const MaxLogSize = 50 << 20

// Log appends entries to a JSON lines file.
type Log struct {
	Path string
	mu   sync.Mutex
}

// Add appends one entry. Errors are ignored: a log failure never fails a
// request.
func (l *Log) Add(e Entry) {
	if l == nil || l.Path == "" {
		return
	}
	if len(e.Error) > 300 {
		e.Error = e.Error[:300]
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := os.Stat(l.Path); err == nil && fi.Size()+int64(len(b)) > MaxLogSize {
		_ = os.Rename(l.Path, l.Path+".1")
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_ = f.Close()
}
