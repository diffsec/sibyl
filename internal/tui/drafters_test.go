package tui

import (
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

func TestModelDetails(t *testing.T) {
	two := 2
	v := newModelsView(nil)
	v.list = client.OrderWithDrafters([]client.Model{
		{Model: models.Model{ID: "big", Size: 1 << 30, Shards: 1}, Drafters: &two},
		{Model: models.Model{ID: "spare", Drafter: true}},
		{Model: models.Model{ID: "dflash", Drafter: true}},
		{Model: models.Model{ID: "qwen"}, Settings: models.Settings{Speculative: models.Speculative{DraftModel: "dflash"}}},
	})
	v.running = client.Running{Models: []client.RunningModel{{ID: "qwen", State: "ready"}}}
	out := ansiRE.ReplaceAllString(v.render(120), "")
	for _, want := range []string{"4 models · 1 loaded", "↳ dflash", "Unused drafters", "none · 2 on Hugging Face · f finds them"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Unused drafters") > strings.Index(out, "spare") {
		t.Errorf("the unused drafter is not under its heading:\n%s", out)
	}
	for i, m := range v.list {
		if m.ID == "spare" {
			v.cursor = i
		}
	}
	if d := ansiRE.ReplaceAllString(v.details(v.list[v.cursor]), ""); !strings.Contains(d, "no model uses it") {
		t.Errorf("unused drafter details:\n%s", d)
	}
}

func TestBenchVerdict(t *testing.T) {
	b := func(hip, vulkan float64) string {
		return ansiRE.ReplaceAllString(benchLine(&client.Benchmark{Status: "done", Results: map[string]client.BackendResult{
			"hip": {GenTPS: hip}, "vulkan": {GenTPS: vulkan}}}), "")
	}
	if got := b(75.3, 75.6); !strings.Contains(got, "within 5%") || strings.Contains(got, "faster") {
		t.Errorf("a 0.4%% gap: %s", got)
	}
	if got := b(62, 27.6); !strings.Contains(got, "hip is 125% faster") {
		t.Errorf("a large gap: %s", got)
	}
}
