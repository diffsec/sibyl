package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/models"
)

// MarshalJSON writes a route as its string form.
func (r Route) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// RuleResult is the outcome of one rule, for explain.
type RuleResult struct {
	Name    string `json:"name"`
	Route   string `json:"route"`
	Matched bool   `json:"matched"`
	// Reason is the first condition that did not match.
	Reason string `json:"reason,omitempty"`
}

// Decision is where the router sends a request, and why.
type Decision struct {
	Requested string  `json:"requested"`
	Profile   string  `json:"profile,omitempty"`
	Rule      string  `json:"rule,omitempty"`
	Route     Route   `json:"route"`
	Fallback  []Route `json:"fallback,omitempty"`
	// Features and Rules are filled for a profile, for explain.
	Features *Features    `json:"features,omitempty"`
	Rules    []RuleResult `json:"rules,omitempty"`
	// Classification is the classifier's result, when a rule needed it.
	Classification *Classification `json:"classification,omitempty"`
	// Price is the price of a provider route. Only explain fills it.
	Price *Price `json:"price,omitempty"`
	// LocalWait limits the wait for a local route when a next route exists.
	LocalWait time.Duration `json:"-"`
	// LocalModel is the profile's local model, the last resort of a client
	// whose budget is spent.
	LocalModel string `json:"-"`
	// Preset is the sampling preset of <model>:<preset>, and Sampling its
	// values, which fill the fields that the request leaves out.
	Preset   string           `json:"preset,omitempty"`
	Sampling *models.Sampling `json:"-"`
}

// Attempts returns the routes in the order that the router tries them.
func (d Decision) Attempts() []Route { return append([]Route{d.Route}, d.Fallback...) }

// statusError is a request that the router refuses.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func refuse(code int, format string, args ...any) error {
	return &statusError{code: code, msg: fmt.Sprintf(format, args...)}
}

// defaultLocalWait is the wait for a local model before the next route.
const defaultLocalWait = 30 * time.Second

// hint reads the X-Sibyl-Route request header. A client can always narrow
// a request to local. It can ask for cloud only when its policy allows hints.
func hint(h http.Header, c Client) string {
	switch v := strings.ToLower(strings.TrimSpace(h.Get("X-Sibyl-Route"))); v {
	case "local":
		return v
	case "cloud":
		if c.AllowHint {
			return v
		}
	}
	return "auto"
}

// staysLocal reports whether a request must not leave the host.
func staysLocal(c Client, hint string) bool { return c.Policy != Hybrid || hint == "local" }

// resolver holds what a decision needs besides the request.
type resolver struct {
	cfg    *Config
	client Client
	hint   string
	// ready reports whether llama-swap has the local model loaded. It is
	// called only when a rule tests local_ready.
	ready func(model string) bool
	// classify labels the text of a request. It is called only when a rule
	// tests task or difficulty.
	classify func(cc ClassifierConfig, text string) *Classification
}

// decide picks the route for a request that names model. f is the profile
// of the request, used when model names a profile.
func (rs resolver) decide(model string, f Features) (Decision, error) {
	d := Decision{Requested: model}
	local := staysLocal(rs.client, rs.hint)
	name, isProfile := profileName(model)
	if !isProfile {
		if p, m, ok := strings.Cut(model, "/"); ok {
			if _, known := rs.cfg.Providers.Providers[p]; known {
				if local {
					return d, refuse(http.StatusForbidden, "client %q must stay on this host, so it cannot use %s", rs.client.Name, model)
				}
				d.Route = Route{Provider: p, Model: m}
				return d, nil
			}
			// Local model IDs have no slash, so the name must be a provider's.
			return d, refuse(http.StatusNotFound, "no provider %q: name a local model, <provider>/<model> or auto", p)
		}
		// <model>:<preset> runs a local model with a sampling preset.
		if base, preset, ok := strings.Cut(model, ":"); ok {
			if lm, known := rs.cfg.localModel(base); known {
				if ps, ok := lm.Presets[preset]; ok {
					d.Route, d.Preset, d.Sampling = Route{Model: base}, preset, &ps
					return d, nil
				}
				return d, refuse(http.StatusNotFound, "model %s has no preset %q", base, preset)
			}
		}
		d.Route = Route{Model: model}
		return d, nil
	}

	p, ok := rs.cfg.Profiles.Profiles[name]
	if !ok {
		return d, refuse(http.StatusNotFound, "no routing profile %q", name)
	}
	d.Profile, d.LocalModel = name, p.LocalModel
	f.Client, f.Policy, f.Hint = rs.client.Name, rs.client.Policy, rs.hint
	if lm, ok := rs.cfg.localModel(p.LocalModel); ok {
		f.LocalContext = lm.Context
	}
	if rs.ready != nil && p.usesLocalReady() {
		f.LocalReady = rs.ready(p.LocalModel)
	}
	if rs.classify != nil && p.usesLabels() {
		d.Classification = rs.classify(rs.cfg.Profiles.Classifier, f.query)
		f.Task, f.Difficulty = d.Classification.Task, d.Classification.Difficulty
	}
	d.Features = &f

	d.Route = Route{Model: p.LocalModel}
	d.Rule = "no rule matched"
	decided := false
	for _, r := range p.Rules {
		res := RuleResult{Name: r.Name, Route: r.Route}
		route, err := ParseRoute(r.Route, p.LocalModel, rs.cfg.Providers)
		switch {
		case err != nil:
			res.Reason = err.Error()
		case decided:
			res.Reason = "an earlier rule decided"
		default:
			res.Reason = r.When.match(f)
			if res.Reason == "" && local && !route.Local() {
				res.Reason = "the route leaves the host, and this request must stay on it"
			}
		}
		if res.Reason == "" {
			res.Matched, decided = true, true
			d.Route, d.Rule = route, r.Name
		}
		d.Rules = append(d.Rules, res)
	}

	for _, s := range p.Fallback {
		route, err := ParseRoute(s, p.LocalModel, rs.cfg.Providers)
		if err != nil || (local && !route.Local()) || route == d.Route || slices.Contains(d.Fallback, route) {
			continue
		}
		d.Fallback = append(d.Fallback, route)
	}
	d.LocalWait = defaultLocalWait
	if p.LocalWaitSeconds > 0 {
		d.LocalWait = time.Duration(p.LocalWaitSeconds) * time.Second
	}
	return d, nil
}

// profileName returns the profile that a model name asks for.
func profileName(model string) (string, bool) {
	if model == "auto" {
		return "auto", true
	}
	if p, ok := strings.CutPrefix(model, "auto:"); ok {
		return p, true
	}
	return "", false
}

func (p Profile) usesLabels() bool {
	for _, r := range p.Rules {
		if len(r.When.Task) > 0 || len(r.When.Difficulty) > 0 {
			return true
		}
	}
	return false
}

func (p Profile) usesLocalReady() bool {
	for _, r := range p.Rules {
		if r.When.LocalReady != nil {
			return true
		}
	}
	return false
}

// match returns "" when every condition matches f, or a description of the
// first condition that does not.
func (w When) match(f Features) string {
	type boolCond struct {
		name string
		want *bool
		got  bool
	}
	for _, c := range []boolCond{
		{"sensitive", w.Sensitive, f.Sensitive},
		{"has_images", w.HasImages, f.HasImages},
		{"has_tools", w.HasTools, f.HasTools},
		{"json_schema", w.JSONSchema, f.JSONSchema},
		{"stream", w.Stream, f.Stream},
		{"local_ready", w.LocalReady, f.LocalReady},
	} {
		if c.want != nil && *c.want != c.got {
			return fmt.Sprintf("%s is %t", c.name, c.got)
		}
	}
	if t := w.PromptTokensOver; t != nil {
		limit := t.N
		if t.LocalContext {
			limit = f.LocalContext
			if limit == 0 {
				return "the local model's context size is not known"
			}
		}
		if f.PromptTokens <= limit {
			return fmt.Sprintf("prompt_tokens %d is not over %d", f.PromptTokens, limit)
		}
	}
	if w.MaxTokensOver != nil && f.MaxTokens <= *w.MaxTokensOver {
		return fmt.Sprintf("max_tokens %d is not over %d", f.MaxTokens, *w.MaxTokensOver)
	}
	for _, c := range []struct {
		name string
		want []string
		got  string
	}{
		{"task", w.Task, f.Task},
		{"difficulty", w.Difficulty, f.Difficulty},
		{"hint", w.Hint, f.Hint},
		{"client", w.Clients, f.Client},
	} {
		if len(c.want) > 0 && !slices.Contains(c.want, c.got) {
			return fmt.Sprintf("%s is %s", c.name, c.got)
		}
	}
	return ""
}
