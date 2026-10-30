package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// The thresholds of a rule proposal.
const (
	// ProposalComparisons is the number of comparisons that a cell needs
	// before it gets a proposal.
	ProposalComparisons = 50
	// ProposeLocalRate moves a cell to the local model when the local
	// answer won or tied at least this often.
	ProposeLocalRate = 0.9
	// ProposeProviderRate moves a cell to a provider when the local answer
	// won or tied less often than this.
	ProposeProviderRate = 0.6
)

// Proposal is a rule change that the calibration results support.
type Proposal struct {
	ID          string  `json:"id"`
	Profile     string  `json:"profile"`
	Task        string  `json:"task"`
	Difficulty  string  `json:"difficulty"`
	Current     string  `json:"current"`
	Route       string  `json:"route"`
	Comparisons int     `json:"comparisons"`
	LocalOKRate float64 `json:"local_ok_rate"`
	Rule        Rule    `json:"rule"`
	// Allowed is true when the profile already names the route, so the
	// automatic mode can apply the proposal.
	Allowed bool `json:"allowed"`
}

// labelRoute is the route that a profile gives a short request with the
// labels, from a hybrid client that sends no hint.
func labelRoute(cfg *Config, profile, task, difficulty string) (Route, error) {
	rs := resolver{cfg: cfg, client: Client{Name: "calibration", Policy: Hybrid},
		ready: func(string) bool { return true },
		classify: func(ClassifierConfig, string) *Classification {
			return &Classification{Task: task, Difficulty: difficulty}
		}}
	model := "auto"
	if profile != "auto" {
		model = "auto:" + profile
	}
	d, err := rs.decide(model, Features{PromptTokens: 100, query: "calibration"})
	return d.Route, err
}

// routeIn reports whether the profile names the route in a rule or in its
// fallback list.
func routeIn(cfg *Config, p Profile, rt Route) bool {
	routes := slices.Clone(p.Fallback)
	for _, r := range p.Rules {
		routes = append(routes, r.Route)
	}
	for _, s := range routes {
		if got, err := ParseRoute(s, p.LocalModel, cfg.Providers); err == nil && got == rt {
			return true
		}
	}
	return false
}

// Propose returns the rule changes that the report supports for the
// profiles. A cell gets a proposal when it has enough comparisons for the
// profile's local model and its rate is past a threshold on the wrong
// side of its current route.
func Propose(cfg *Config, r CalibrationReport) []Proposal {
	out := []Proposal{}
	for _, name := range sortedKeys(cfg.Profiles.Profiles) {
		p := cfg.Profiles.Profiles[name]
		for _, c := range r.Cells {
			if c.LocalModel != p.LocalModel || c.Comparisons < ProposalComparisons ||
				c.Task == "unknown" || c.Difficulty == "unknown" {
				continue
			}
			cur, err := labelRoute(cfg, name, c.Task, c.Difficulty)
			if err != nil {
				continue
			}
			var to Route
			switch {
			case !cur.Local() && c.LocalOKRate >= ProposeLocalRate:
				to = Route{Model: p.LocalModel}
			case cur.Local() && c.LocalOKRate < ProposeProviderRate:
				rt, ok := compareRoute(cfg, name)
				if !ok || rt.Local() {
					continue
				}
				to = rt
			default:
				continue
			}
			out = append(out, newProposal(cfg, name, p, c, cur, to))
		}
	}
	return out
}

func newProposal(cfg *Config, name string, p Profile, c CalibrationCell, cur, to Route) Proposal {
	route := to.String()
	rule := Rule{Name: fmt.Sprintf("calibration: %s %s", c.Task, c.Difficulty),
		When: When{Task: []string{c.Task}, Difficulty: []string{c.Difficulty}}}
	if to.Local() {
		rule.Route = "local"
		route = "local"
	} else {
		rule.Route = to.Provider + "/" + to.Model
		// A sensitive request never leaves the host by a calibration rule,
		// even when the rule lands before the profile's sensitive rule.
		rule.When.Sensitive = new(bool)
	}
	h := sha256.Sum256([]byte(name + "\x00" + c.Task + "\x00" + c.Difficulty + "\x00" + route))
	return Proposal{
		ID: hex.EncodeToString(h[:6]), Profile: name, Task: c.Task, Difficulty: c.Difficulty,
		Current: cur.String(), Route: route, Comparisons: c.Comparisons, LocalOKRate: c.LocalOKRate,
		Rule: rule, Allowed: to.Local() || routeIn(cfg, p, to),
	}
}

// Apply adds a proposal's rule to its profile, in place of an older
// calibration rule for the same labels. The rule goes before the first
// rule that tests labels, or before a last rule that matches everything,
// so the rules for sensitive and long requests still come first.
func Apply(f *ProfilesFile, prop Proposal) error {
	p, ok := f.Profiles[prop.Profile]
	if !ok {
		return fmt.Errorf("no profile %q", prop.Profile)
	}
	rules := slices.DeleteFunc(slices.Clone(p.Rules), func(r Rule) bool { return r.Name == prop.Rule.Name })
	at := slices.IndexFunc(rules, func(r Rule) bool {
		return len(r.When.Task) > 0 || len(r.When.Difficulty) > 0 || r.When.NeedsProvider != nil
	})
	if at < 0 {
		at = len(rules)
		if at > 0 && rules[at-1].When.empty() {
			at--
		}
	}
	p.Rules = slices.Insert(rules, at, prop.Rule)
	f.Profiles[prop.Profile] = p
	return nil
}

// Text describes the conditions in one line, or "always" for none.
func (w When) Text() string {
	var parts []string
	flag := func(name string, b *bool) {
		if b != nil {
			parts = append(parts, fmt.Sprintf("%s=%t", name, *b))
		}
	}
	flag("sensitive", w.Sensitive)
	flag("images", w.HasImages)
	flag("tools", w.HasTools)
	flag("json_schema", w.JSONSchema)
	flag("stream", w.Stream)
	flag("local_ready", w.LocalReady)
	flag("needs_provider", w.NeedsProvider)
	if w.PromptTokensOver != nil {
		parts = append(parts, "prompt>"+w.PromptTokensOver.String())
	}
	if w.MaxTokensOver != nil {
		parts = append(parts, fmt.Sprintf("max_tokens>%d", *w.MaxTokensOver))
	}
	for _, l := range []struct {
		name string
		vals []string
	}{{"task", w.Task}, {"difficulty", w.Difficulty}, {"hint", w.Hint}, {"clients", w.Clients}} {
		if len(l.vals) > 0 {
			parts = append(parts, l.name+"="+strings.Join(l.vals, ","))
		}
	}
	if len(parts) == 0 {
		return "always"
	}
	return strings.Join(parts, " ")
}

func (w When) empty() bool {
	return w.Sensitive == nil && w.HasImages == nil && w.HasTools == nil && w.JSONSchema == nil &&
		w.Stream == nil && w.LocalReady == nil && w.PromptTokensOver == nil && w.MaxTokensOver == nil &&
		len(w.Task) == 0 && len(w.Difficulty) == 0 && len(w.Hint) == 0 && len(w.Clients) == 0 &&
		w.NeedsProvider == nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
