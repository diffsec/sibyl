// Package catalog reads the model index of models.dev: providers, their
// API base URLs, and each model's prices, limits and features. The System
// API fetches it once a day and keeps a trimmed copy, which the router
// reads for prices and the TUI uses to set up providers.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultURL is the models.dev index.
const DefaultURL = "https://models.dev/api.json"

// FileName is the trimmed copy in the router's configuration directory.
const FileName = "catalog.json"

// The provider types that the router speaks, as in router.ProviderType.
const (
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
)

// Tier is a price that applies above a prompt size.
type Tier struct {
	Above      int     `json:"above"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Cost is a model's price in US dollars per million tokens.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	Tiers      []Tier  `json:"tiers,omitempty"`
}

// Model is one model of a provider.
type Model struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Context    int      `json:"context,omitempty"`
	Output     int      `json:"output,omitempty"`
	ToolCall   bool     `json:"tool_call,omitempty"`
	Reasoning  bool     `json:"reasoning,omitempty"`
	Attachment bool     `json:"attachment,omitempty"`
	Input      []string `json:"input,omitempty"` // modalities, such as text and image
	Cost       *Cost    `json:"cost,omitempty"`
}

// Provider is one provider that the router can use.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is the router's provider type: openai or anthropic.
	Type    string           `json:"type"`
	BaseURL string           `json:"base_url"`
	Doc     string           `json:"doc,omitempty"`
	Env     []string         `json:"env,omitempty"`
	Models  map[string]Model `json:"models"`
}

// Catalog is the trimmed index.
type Catalog struct {
	Fetched   time.Time           `json:"fetched"`
	Source    string              `json:"source"`
	Providers map[string]Provider `json:"providers"`
}

// wire is the models.dev format, as far as this package reads it.
type wire map[string]struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	NPM    string   `json:"npm"`
	API    string   `json:"api"`
	Doc    string   `json:"doc"`
	Env    []string `json:"env"`
	Models map[string]struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ToolCall   bool   `json:"tool_call"`
		Reasoning  bool   `json:"reasoning"`
		Attachment bool   `json:"attachment"`
		Modalities struct {
			Input []string `json:"input"`
		} `json:"modalities"`
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
		Cost *struct {
			Input      float64 `json:"input"`
			Output     float64 `json:"output"`
			CacheRead  float64 `json:"cache_read"`
			CacheWrite float64 `json:"cache_write"`
			Tiers      []struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
				Tier       struct {
					Type string `json:"type"`
					Size int    `json:"size"`
				} `json:"tier"`
			} `json:"tiers"`
		} `json:"cost"`
	} `json:"models"`
}

// providerType returns the router type and base URL of a models.dev
// provider, or ok false when the router cannot speak its API.
func providerType(id, npm, api string) (typ, base string, ok bool) {
	api = strings.TrimSuffix(api, "/")
	switch {
	case npm == "@ai-sdk/anthropic":
		if api == "" {
			api = "https://api.anthropic.com"
		}
		// The Anthropic SDK adds /v1/messages to the host.
		return TypeAnthropic, strings.TrimSuffix(api, "/v1"), strings.HasPrefix(api, "https://")
	case id == "openai" && api == "":
		return TypeOpenAI, "https://api.openai.com/v1", true
	case npm == "@ai-sdk/openai-compatible" || npm == "@ai-sdk/openai" || npm == "@openrouter/ai-sdk-provider":
		return TypeOpenAI, api, strings.HasPrefix(api, "https://")
	}
	return "", "", false
}

// Parse reads the models.dev index and keeps the providers that the
// router can use.
func Parse(raw []byte, source string, now time.Time) (Catalog, error) {
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Catalog{}, fmt.Errorf("read the models.dev index: %w", err)
	}
	c := Catalog{Fetched: now.UTC(), Source: source, Providers: map[string]Provider{}}
	for id, p := range w {
		typ, base, ok := providerType(id, p.NPM, p.API)
		if !ok {
			continue
		}
		out := Provider{ID: id, Name: p.Name, Type: typ, BaseURL: base, Doc: p.Doc, Env: p.Env, Models: map[string]Model{}}
		for mid, m := range p.Models {
			om := Model{ID: mid, Name: m.Name, Context: m.Limit.Context, Output: m.Limit.Output,
				ToolCall: m.ToolCall, Reasoning: m.Reasoning, Attachment: m.Attachment, Input: m.Modalities.Input}
			if m.Cost != nil {
				cost := &Cost{Input: m.Cost.Input, Output: m.Cost.Output, CacheRead: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite}
				for _, t := range m.Cost.Tiers {
					if t.Tier.Type == "context" && t.Tier.Size > 0 {
						cost.Tiers = append(cost.Tiers, Tier{Above: t.Tier.Size, Input: t.Input, Output: t.Output,
							CacheRead: t.CacheRead, CacheWrite: t.CacheWrite})
					}
				}
				sort.Slice(cost.Tiers, func(i, j int) bool { return cost.Tiers[i].Above < cost.Tiers[j].Above })
				om.Cost = cost
			}
			out.Models[mid] = om
		}
		c.Providers[id] = out
	}
	if len(c.Providers) == 0 {
		return Catalog{}, errors.New("the models.dev index has no provider that Sibyl can use")
	}
	return c, nil
}

// Load reads a trimmed catalog file. A missing file gives an empty
// catalog.
func Load(path string) (Catalog, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Catalog{Providers: map[string]Provider{}}, nil
	}
	if err != nil {
		return Catalog{}, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return Catalog{}, fmt.Errorf("%s: %w", FileName, err)
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	return c, nil
}

// Model returns a model of a provider.
func (c Catalog) Model(provider, model string) (Model, bool) {
	p, ok := c.Providers[provider]
	if !ok {
		return Model{}, false
	}
	m, ok := p.Models[model]
	return m, ok
}

// Sorted returns the providers in name order.
func (c Catalog) Sorted() []Provider {
	out := make([]Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// SortedModels returns a provider's models in ID order.
func (p Provider) SortedModels() []Model {
	out := make([]Model, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
