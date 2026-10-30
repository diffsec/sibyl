package router

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

const proposeProfiles = `profiles:
  auto:
    local_model: small
    rules:
      - name: secrets stay
        when: {sensitive: true}
        route: local
      - name: too long
        when: {prompt_tokens_over: local_context}
        route: cloud/big
      - name: hard to the provider
        when: {difficulty: [hard]}
        route: cloud/big
      - name: everything else
        route: local
`

func proposeConfig(t *testing.T, profiles string) *Config {
	t.Helper()
	cfg := &Config{Providers: ProvidersFile{Providers: map[string]Provider{
		"cloud": {Type: ProviderOpenAI, BaseURL: "https://x", Models: []string{"big"}},
	}}}
	if err := yaml.Unmarshal([]byte(profiles), &cfg.Profiles); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Profiles.Validate(cfg.Providers); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestPropose(t *testing.T) {
	cfg := proposeConfig(t, proposeProfiles)
	cell := func(model, task, diff string, n int, rate float64) CalibrationCell {
		return CalibrationCell{LocalModel: model, Task: task, Difficulty: diff, Comparisons: n, LocalOKRate: rate}
	}
	got := Propose(cfg, CalibrationReport{Cells: []CalibrationCell{
		cell("small", "code", "hard", 60, 0.95),   // provider now, local wins: to local
		cell("small", "code", "medium", 60, 0.4),  // local now, local loses: to the provider
		cell("small", "chat", "easy", 60, 0.95),   // local now and fine
		cell("small", "math", "hard", 60, 0.7),    // provider now, local not good enough
		cell("small", "math", "medium", 10, 0.1),  // too few comparisons
		cell("other", "code", "medium", 99, 0.1),  // another local model
		cell("small", "unknown", "medium", 99, 0), // no label
	}})
	if len(got) != 2 {
		t.Fatalf("proposals %+v, want 2", got)
	}
	toLocal, toCloud := got[0], got[1]
	if toLocal.Task != "code" || toLocal.Difficulty != "hard" || toLocal.Current != "cloud/big" || toLocal.Route != "local" || !toLocal.Allowed {
		t.Errorf("to local %+v", toLocal)
	}
	if toCloud.Difficulty != "medium" || toCloud.Current != "local/small" || toCloud.Route != "cloud/big" || !toCloud.Allowed {
		t.Errorf("to cloud %+v", toCloud)
	}
	if s := toCloud.Rule.When.Sensitive; s == nil || *s {
		t.Errorf("a provider rule must exclude sensitive requests: %+v", toCloud.Rule.When)
	}

	f := cfg.Profiles
	for _, p := range got {
		if err := Apply(&f, p); err != nil {
			t.Fatal(err)
		}
	}
	// Applying again replaces the rule rather than adding a second one.
	if err := Apply(&f, toCloud); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range f.Profiles["auto"].Rules {
		names = append(names, r.Name)
	}
	want := []string{"secrets stay", "too long", "calibration: code medium", "calibration: code hard", "hard to the provider", "everything else"}
	if len(names) != len(want) {
		t.Fatalf("rules %q, want %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("rules %q, want %q", names, want)
		}
	}
	applied := &Config{Providers: cfg.Providers, Profiles: f}
	for _, c := range []struct{ task, diff, want string }{
		{"code", "hard", "local/small"}, {"code", "medium", "cloud/big"}, {"math", "hard", "cloud/big"}, {"chat", "easy", "local/small"},
	} {
		if rt, err := labelRoute(applied, "auto", c.task, c.diff); err != nil || rt.String() != c.want {
			t.Errorf("%s %s: route %s %v, want %s", c.task, c.diff, rt, err, c.want)
		}
	}
	if left := Propose(applied, CalibrationReport{Cells: []CalibrationCell{cell("small", "code", "hard", 60, 0.95)}}); len(left) != 0 {
		t.Errorf("an applied proposal is proposed again: %+v", left)
	}
}

func TestApplyWithNoLabelRules(t *testing.T) {
	f := proposeConfig(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: secrets stay
        when: {sensitive: true}
        route: local
      - name: everything else
        route: local
`).Profiles
	err := Apply(&f, Proposal{Profile: "auto", Rule: Rule{Name: "calibration: code hard", Route: "cloud/big",
		When: When{Task: []string{"code"}, Difficulty: []string{"hard"}, Sensitive: new(bool)}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Profiles["auto"].Rules; len(r) != 3 || r[1].Name != "calibration: code hard" {
		t.Errorf("rules %+v, want the new rule before the catch-all", r)
	}
	if err := Apply(&f, Proposal{Profile: "nope"}); err == nil {
		t.Error("a missing profile applied")
	}
}

func TestProposalNotAllowed(t *testing.T) {
	cfg := proposeConfig(t, `profiles:
  auto:
    local_model: small
    rules:
      - name: everything else
        route: local
`)
	cfg.Profiles.Calibration.Compare = "cloud/big"
	got := Propose(cfg, CalibrationReport{Cells: []CalibrationCell{{LocalModel: "small", Task: "code", Difficulty: "hard", Comparisons: 50, LocalOKRate: 0.2}}})
	if len(got) != 1 || got[0].Allowed {
		t.Errorf("proposals %+v, want one that the profile does not allow", got)
	}
}
