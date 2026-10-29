package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// index is a small models.dev index with one provider of each kind.
const index = `{
 "anthropic": {"id":"anthropic","name":"Anthropic","npm":"@ai-sdk/anthropic","env":["ANTHROPIC_API_KEY"],"models":{
   "claude-haiku-5-5":{"id":"claude-haiku-5-5","name":"Claude Haiku 5.5","tool_call":true,"limit":{"context":1000000,"output":128000},
     "cost":{"input":0.1,"output":0.5,"cache_read":0.01,"cache_write":0.125,
       "tiers":[{"input":0.5,"output":2.5,"cache_read":0.05,"cache_write":0.625,"tier":{"type":"context","size":100000}}]}}}},
 "openai": {"id":"openai","name":"OpenAI","npm":"@ai-sdk/openai","models":{"gpt-x":{"id":"gpt-x","name":"GPT X","cost":{"input":1,"output":4}}}},
 "orcarouter": {"id":"orcarouter","name":"OrcaRouter","npm":"@ai-sdk/openai-compatible","api":"https://api.orcarouter.ai/v1/","models":{
   "deepseek/deepseek-v4-flash":{"id":"deepseek/deepseek-v4-flash","name":"DeepSeek V4 Flash","modalities":{"input":["text","image"]}}}},
 "azure": {"id":"azure","name":"Azure","npm":"@ai-sdk/azure","models":{}},
 "plain-http": {"id":"plain-http","name":"Plain","npm":"@ai-sdk/openai-compatible","api":"http://insecure.example/v1","models":{}}
}`

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	c, err := Parse([]byte(index), DefaultURL, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 3 {
		t.Fatalf("providers %v: azure and plain http must be left out", c.Sorted())
	}
	for id, want := range map[string][2]string{
		"anthropic":  {TypeAnthropic, "https://api.anthropic.com"},
		"openai":     {TypeOpenAI, "https://api.openai.com/v1"},
		"orcarouter": {TypeOpenAI, "https://api.orcarouter.ai/v1"},
	} {
		if p := c.Providers[id]; p.Type != want[0] || p.BaseURL != want[1] {
			t.Errorf("%s: %s %s", id, p.Type, p.BaseURL)
		}
	}
	m, ok := c.Model("anthropic", "claude-haiku-5-5")
	if !ok || m.Context != 1000000 || len(m.Cost.Tiers) != 1 || m.Cost.Tiers[0].Above != 100000 || m.Cost.Tiers[0].Input != 0.5 {
		t.Errorf("haiku %+v", m)
	}
	if m, _ := c.Model("orcarouter", "deepseek/deepseek-v4-flash"); m.Cost != nil || len(m.Input) != 2 {
		t.Errorf("a model with no price %+v", m)
	}

	path := filepath.Join(t.TempDir(), FileName)
	if got, err := Load(path); err != nil || len(got.Providers) != 0 {
		t.Errorf("missing file: %v %v", got, err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a broken file loaded")
	}
	if _, err := Parse([]byte(`{"azure":{"npm":"@ai-sdk/azure","models":{}}}`), DefaultURL, now); err == nil {
		t.Error("an index with no usable provider parsed")
	}
}
