// Package router is the Sibyl model router. It checks the client key, picks
// a route for each request (a local model or a provider), and forwards the
// request. docs/router-design.md describes the design.
package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/diffsec/sibyl/internal/catalog"
	"github.com/diffsec/sibyl/internal/models"
)

// Policy decides whether a client's requests can leave the host.
type Policy string

// The client policies.
const (
	// LocalOnly clients never reach a provider.
	LocalOnly Policy = "local-only"
	// Hybrid clients follow the rules.
	Hybrid Policy = "hybrid"
)

// The clients whose keys come from Podman secrets, not from clients.yaml.
const (
	// DefaultClient holds the key that the first boot creates.
	DefaultClient = "default"
	// OpenWebUIClient is Open WebUI.
	OpenWebUIClient = "open-webui"
)

// Builtin reports whether name is a client whose key is a Podman secret.
func Builtin(name string) bool { return name == DefaultClient || name == OpenWebUIClient }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName checks a client, provider or profile name.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("name %q: use 1 to 32 lowercase letters, digits and dashes", name)
	}
	return nil
}

// Client is one inference client.
type Client struct {
	Name string `yaml:"name" json:"name"`
	// KeySHA256 is the hex SHA-256 of the client's key. The built-in
	// clients have none, because their keys are Podman secrets.
	KeySHA256 string `yaml:"key_sha256,omitempty" json:"-"`
	Policy    Policy `yaml:"policy" json:"policy"`
	// AllowHint lets the client ask for a provider with the X-Sibyl-Route
	// request header.
	AllowHint bool `yaml:"allow_hint,omitempty" json:"allow_hint,omitempty"`
	// Budget limits the client's provider spend. Nil means no limit.
	Budget *Budget `yaml:"budget,omitempty" json:"budget,omitempty"`
	// Calibrate lets the router keep a sample of the client's requests and
	// answers, to compare the local model with a provider. Only a hybrid
	// client's requests are sampled.
	Calibrate bool `yaml:"calibrate,omitempty" json:"calibrate,omitempty"`
}

// ClientsFile is clients.yaml.
type ClientsFile struct {
	Clients []Client `yaml:"clients" json:"clients"`
}

var hexKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the clients.
func (f ClientsFile) Validate() error {
	seen := map[string]bool{}
	for _, c := range f.Clients {
		if err := ValidName(c.Name); err != nil {
			return fmt.Errorf("client: %w", err)
		}
		if seen[c.Name] {
			return fmt.Errorf("client %q is listed twice", c.Name)
		}
		seen[c.Name] = true
		if c.Policy != LocalOnly && c.Policy != Hybrid {
			return fmt.Errorf("client %q: policy must be %s or %s", c.Name, LocalOnly, Hybrid)
		}
		if b := c.Budget; b != nil && (b.DailyUSD < 0 || b.MonthlyUSD < 0) {
			return fmt.Errorf("client %q: a budget must not be negative", c.Name)
		}
		switch {
		case Builtin(c.Name) && c.KeySHA256 != "":
			return fmt.Errorf("client %q: the key of a built-in client is a Podman secret", c.Name)
		case !Builtin(c.Name) && !hexKey.MatchString(c.KeySHA256):
			return fmt.Errorf("client %q: key_sha256 must be 64 lowercase hex digits", c.Name)
		}
	}
	return nil
}

// ProviderType is the API that a provider speaks.
type ProviderType string

// The provider types.
const (
	// ProviderOpenAI is any provider with an OpenAI-compatible API, such as
	// OpenAI and OpenRouter.
	ProviderOpenAI ProviderType = "openai"
	// ProviderAnthropic is the Anthropic Messages API.
	ProviderAnthropic ProviderType = "anthropic"
)

// AnthropicBaseURL is the default base URL of the type anthropic.
const AnthropicBaseURL = "https://api.anthropic.com"

// Provider is one frontier model provider. Its key is the file
// keys/<name> in the configuration directory.
type Provider struct {
	Type ProviderType `yaml:"type" json:"type"`
	// BaseURL ends before the endpoint path, such as
	// https://api.openai.com/v1. For the type anthropic it is the host, and
	// the default is https://api.anthropic.com.
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	// Models are the model names that GET /v1/models lists for this
	// provider. A client can name any other model of the provider too.
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`
	// Prices by model name. They override the models.dev prices and the
	// built-in Anthropic prices.
	Prices map[string]Price `yaml:"prices,omitempty" json:"prices,omitempty"`
	// Catalog is the provider's ID in the models.dev index, for prices.
	// Empty means the provider's own name.
	Catalog string `yaml:"catalog,omitempty" json:"catalog,omitempty"`
}

// URL returns the base URL, with the default for the type.
func (p Provider) URL() string {
	if p.BaseURL == "" && p.Type == ProviderAnthropic {
		return AnthropicBaseURL
	}
	return strings.TrimSuffix(p.BaseURL, "/")
}

// ProvidersFile is providers.yaml.
type ProvidersFile struct {
	Providers map[string]Provider `yaml:"providers" json:"providers"`
}

// reserved names cannot be providers, because a route names them.
var reserved = map[string]bool{"local": true, "auto": true}

// Validate checks the providers.
func (f ProvidersFile) Validate() error {
	for name, p := range f.Providers {
		if err := ValidName(name); err != nil {
			return fmt.Errorf("provider: %w", err)
		}
		if reserved[name] {
			return fmt.Errorf("provider %q: the name is reserved", name)
		}
		if p.Type != ProviderOpenAI && p.Type != ProviderAnthropic {
			return fmt.Errorf("provider %q: type must be %s or %s", name, ProviderOpenAI, ProviderAnthropic)
		}
		if !strings.HasPrefix(p.URL(), "https://") {
			return fmt.Errorf("provider %q: base_url must start with https://", name)
		}
		for m, pr := range p.Prices {
			if err := pr.validate(); err != nil {
				return fmt.Errorf("provider %q price of %q: %w", name, m, err)
			}
		}
		for _, m := range p.Models {
			if m == "" || strings.ContainsAny(m, " \t\r\n") {
				return fmt.Errorf("provider %q: model %q is not valid", name, m)
			}
		}
	}
	return nil
}

// Tokens is a token count in a rule: a number, or the local model's
// context size.
type Tokens struct {
	N            int
	LocalContext bool
}

const localContextWord = "local_context"

func (t *Tokens) parse(s string) error {
	if s == localContextWord {
		*t = Tokens{LocalContext: true}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf("give a token count or %s, not %q", localContextWord, s)
	}
	*t = Tokens{N: n}
	return nil
}

func (t Tokens) String() string {
	if t.LocalContext {
		return localContextWord
	}
	return strconv.Itoa(t.N)
}

// UnmarshalYAML reads a number or local_context.
func (t *Tokens) UnmarshalYAML(n *yaml.Node) error { return t.parse(n.Value) }

// MarshalYAML writes a number or local_context.
func (t Tokens) MarshalYAML() (any, error) {
	if t.LocalContext {
		return localContextWord, nil
	}
	return t.N, nil
}

// UnmarshalJSON reads a number or "local_context".
func (t *Tokens) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return t.parse(s)
	}
	return t.parse(string(b))
}

// MarshalJSON writes a number or "local_context".
func (t Tokens) MarshalJSON() ([]byte, error) {
	if t.LocalContext {
		return json.Marshal(localContextWord)
	}
	return json.Marshal(t.N)
}

// When holds the conditions of a rule. A rule matches when every condition
// that is set matches. A rule with no conditions always matches.
type When struct {
	Sensitive        *bool    `yaml:"sensitive,omitempty" json:"sensitive,omitempty"`
	HasImages        *bool    `yaml:"has_images,omitempty" json:"has_images,omitempty"`
	HasTools         *bool    `yaml:"has_tools,omitempty" json:"has_tools,omitempty"`
	JSONSchema       *bool    `yaml:"json_schema,omitempty" json:"json_schema,omitempty"`
	Stream           *bool    `yaml:"stream,omitempty" json:"stream,omitempty"`
	LocalReady       *bool    `yaml:"local_ready,omitempty" json:"local_ready,omitempty"`
	PromptTokensOver *Tokens  `yaml:"prompt_tokens_over,omitempty" json:"prompt_tokens_over,omitempty"`
	MaxTokensOver    *int     `yaml:"max_tokens_over,omitempty" json:"max_tokens_over,omitempty"`
	Task             []string `yaml:"task,omitempty" json:"task,omitempty"`
	Difficulty       []string `yaml:"difficulty,omitempty" json:"difficulty,omitempty"`
	Hint             []string `yaml:"hint,omitempty" json:"hint,omitempty"`
	Clients          []string `yaml:"clients,omitempty" json:"clients,omitempty"`
	// NeedsProvider tests the vote of the nearest calibration outcomes.
	// It matches neither true nor false while the vote is unknown.
	NeedsProvider *bool `yaml:"needs_provider,omitempty" json:"needs_provider,omitempty"`
}

// Rule is one rule of a profile.
type Rule struct {
	Name  string `yaml:"name" json:"name"`
	When  When   `yaml:"when,omitempty" json:"when,omitzero"`
	Route string `yaml:"route" json:"route"`
}

// Profile is an ordered list of rules for the model name auto or
// auto:<profile>.
type Profile struct {
	// LocalModel is the local model that the route "local" means.
	LocalModel string   `yaml:"local_model" json:"local_model"`
	Rules      []Rule   `yaml:"rules,omitempty" json:"rules,omitempty"`
	Fallback   []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	// LocalWaitSeconds is how long the router waits for a local model to
	// start answering before it tries the next route. It applies only when
	// a next route exists. The default is 30.
	LocalWaitSeconds int `yaml:"local_wait_seconds,omitempty" json:"local_wait_seconds,omitempty"`
}

// ProfilesFile is profiles.yaml.
type ProfilesFile struct {
	// SensitiveWords are words that mark a request as sensitive, in
	// addition to the built-in secret detectors.
	SensitiveWords []string `yaml:"sensitive_words,omitempty" json:"sensitive_words,omitempty"`
	// Classifier sets up the task and difficulty labels.
	Classifier ClassifierConfig `yaml:"classifier,omitempty" json:"classifier,omitzero"`
	// Calibration sets up the comparison of sampled requests.
	Calibration CalibrationConfig  `yaml:"calibration,omitempty" json:"calibration,omitzero"`
	Profiles    map[string]Profile `yaml:"profiles" json:"profiles"`
}

// The labels that the classifier gives, and the hints that a client sends.
var (
	Tasks        = []string{"chat", "code", "math", "writing", "extraction", "unknown"}
	Difficulties = []string{"easy", "medium", "hard", "unknown"}
	Hints        = []string{"local", "cloud", "auto"}
)

// Validate checks the profiles against the providers.
func (f ProfilesFile) Validate(providers ProvidersFile) error {
	for _, w := range f.SensitiveWords {
		if strings.TrimSpace(w) == "" {
			return errors.New("sensitive_words: a word is empty")
		}
	}
	if err := f.Classifier.validate(); err != nil {
		return err
	}
	if err := f.Calibration.validate(providers); err != nil {
		return err
	}
	for name, p := range f.Profiles {
		if err := ValidName(name); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
		if p.LocalModel == "" {
			return fmt.Errorf("profile %q: local_model is required", name)
		}
		if p.LocalWaitSeconds < 0 || p.LocalWaitSeconds > 3600 {
			return fmt.Errorf("profile %q: local_wait_seconds must be 0 to 3600", name)
		}
		for i, r := range p.Rules {
			where := fmt.Sprintf("profile %q rule %d", name, i+1)
			if strings.TrimSpace(r.Name) == "" {
				return fmt.Errorf("%s: name is required", where)
			}
			if _, err := ParseRoute(r.Route, p.LocalModel, providers); err != nil {
				return fmt.Errorf("%s (%s): %w", where, r.Name, err)
			}
			if err := r.When.validate(); err != nil {
				return fmt.Errorf("%s (%s): %w", where, r.Name, err)
			}
		}
		for _, fb := range p.Fallback {
			if _, err := ParseRoute(fb, p.LocalModel, providers); err != nil {
				return fmt.Errorf("profile %q fallback: %w", name, err)
			}
		}
	}
	return nil
}

func (w When) validate() error {
	for _, c := range []struct {
		field string
		got   []string
		allow []string
	}{{"task", w.Task, Tasks}, {"difficulty", w.Difficulty, Difficulties}, {"hint", w.Hint, Hints}} {
		for _, v := range c.got {
			if !slices.Contains(c.allow, v) {
				return fmt.Errorf("%s %q: use one of %s", c.field, v, strings.Join(c.allow, ", "))
			}
		}
	}
	for _, c := range w.Clients {
		if err := ValidName(c); err != nil {
			return fmt.Errorf("clients: %w", err)
		}
	}
	if w.MaxTokensOver != nil && *w.MaxTokensOver < 0 {
		return errors.New("max_tokens_over must not be negative")
	}
	return nil
}

// Route is where a request goes: a local model, or a model of a provider.
type Route struct {
	Provider string // empty for a local model
	Model    string
}

// Local reports whether the route stays on the host.
func (r Route) Local() bool { return r.Provider == "" }

func (r Route) String() string {
	if r.Local() {
		return "local/" + r.Model
	}
	return r.Provider + "/" + r.Model
}

// ParseRoute reads a route of a rule or a fallback list: "local", a local
// model ID, or <provider>/<model>.
func ParseRoute(s, localModel string, providers ProvidersFile) (Route, error) {
	switch s {
	case "":
		return Route{}, errors.New("route is required")
	case "local":
		return Route{Model: localModel}, nil
	}
	if p, m, ok := strings.Cut(s, "/"); ok {
		if _, known := providers.Providers[p]; !known {
			return Route{}, fmt.Errorf("route %q: no provider %q", s, p)
		}
		if m == "" {
			return Route{}, fmt.Errorf("route %q: give the model after the slash", s)
		}
		return Route{Provider: p, Model: m}, nil
	}
	if strings.ContainsAny(s, " \t\r\n:") {
		return Route{}, fmt.Errorf("route %q is not a local model ID", s)
	}
	return Route{Model: s}, nil
}

// LocalModel is what the router knows about one local model. The System
// API writes the list to models.json when it applies the configuration.
type LocalModel struct {
	ID string `json:"id"`
	// Context is the context size that llama-server runs the model with.
	Context   int  `json:"context,omitempty"`
	Embedding bool `json:"embedding,omitempty"`
	// Presets are the sampling of each preset, which a client picks per
	// request with <model>:<preset>.
	Presets map[string]models.Sampling `json:"presets,omitempty"`
}

// Config is the router configuration, read from one directory.
type Config struct {
	Clients   ClientsFile
	Providers ProvidersFile
	Profiles  ProfilesFile
	Models    []LocalModel
	// Keys are the provider keys by provider name.
	Keys map[string]string
	// Catalog is the models.dev index that the System API keeps.
	Catalog catalog.Catalog
	// CatalogErr is why catalog.json could not be read, if it could not.
	CatalogErr error
	// Examples are the owner's classifier examples from examples.jsonl.
	Examples []Example
}

// The file names in the configuration directory.
const (
	ClientsName   = "clients.yaml"
	ProvidersName = "providers.yaml"
	ProfilesName  = "profiles.yaml"
	ModelsName    = "models.json"
	CatalogName   = catalog.FileName
	KeysDir       = "keys"
)

// readYAML decodes path into v. A missing file leaves v as it is. Unknown
// fields are errors, so a typo in a rule does not pass unnoticed.
func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	// An empty file decodes as io.EOF.
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// LoadConfig reads and checks the configuration in dir.
func LoadConfig(dir string) (*Config, error) {
	c := &Config{Keys: map[string]string{}}
	if err := readYAML(filepath.Join(dir, ClientsName), &c.Clients); err != nil {
		return nil, err
	}
	if err := readYAML(filepath.Join(dir, ProvidersName), &c.Providers); err != nil {
		return nil, err
	}
	if err := readYAML(filepath.Join(dir, ProfilesName), &c.Profiles); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, ModelsName)); err == nil {
		if err := json.Unmarshal(b, &c.Models); err != nil {
			return nil, fmt.Errorf("%s: %w", ModelsName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// A broken catalog only costs prices, so it does not stop the router.
	if cat, err := catalog.Load(filepath.Join(dir, CatalogName)); err == nil {
		c.Catalog = cat
	} else {
		c.CatalogErr = err
	}
	if err := c.Clients.Validate(); err != nil {
		return nil, err
	}
	if err := c.Providers.Validate(); err != nil {
		return nil, err
	}
	if err := c.Profiles.Validate(c.Providers); err != nil {
		return nil, err
	}
	ex, err := loadExamples(filepath.Join(dir, ExamplesName))
	if err != nil {
		return nil, err
	}
	c.Examples = ex
	for name := range c.Providers.Providers {
		b, err := os.ReadFile(filepath.Join(dir, KeysDir, name))
		if err != nil {
			return nil, fmt.Errorf("provider %q: read its key: %w", name, err)
		}
		c.Keys[name] = strings.TrimSpace(string(b))
	}
	return c, nil
}

// ProviderNames returns the provider names in order.
func (c *Config) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers.Providers))
	for n := range c.Providers.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// localModel returns the local model with the ID, if the System API listed it.
func (c *Config) localModel(id string) (LocalModel, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return LocalModel{}, false
}
