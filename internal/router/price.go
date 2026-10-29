package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Price is what a provider charges for a model, in US dollars per million
// tokens.
type Price struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read,omitempty" json:"cache_read,omitempty"`
	CacheWrite float64 `yaml:"cache_write,omitempty" json:"cache_write,omitempty"`
	// LongAbove, when set, is a prompt size in tokens. A longer prompt is
	// billed at LongInput and LongOutput, and the cache prices scale with
	// the input price.
	LongAbove  int     `yaml:"long_above,omitempty" json:"long_above,omitempty"`
	LongInput  float64 `yaml:"long_input,omitempty" json:"long_input,omitempty"`
	LongOutput float64 `yaml:"long_output,omitempty" json:"long_output,omitempty"`
}

func (p Price) validate() error {
	for _, v := range []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.LongInput, p.LongOutput} {
		if v < 0 {
			return errors.New("a price is negative")
		}
	}
	if p.LongAbove < 0 {
		return errors.New("long_above is negative")
	}
	return nil
}

// AnthropicPricesDate is the day the built-in Anthropic prices were taken
// from Anthropic's model list. The owner can override any of them in
// providers.yaml.
const AnthropicPricesDate = "2026-10-29"

// AnthropicPrices are the built-in prices for the type anthropic. Where the
// list gave no cache price, a cache read is 0.1 and a five-minute cache
// write 1.25 times the input price, as Anthropic's caching docs state.
var AnthropicPrices = map[string]Price{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-haiku-5-5": {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125,
		LongAbove: 100_000, LongInput: 0.50, LongOutput: 2.50},
}

// price returns the price of a model of a provider: the provider's own
// table first, then the built-in table of its type.
func (c *Config) price(rt Route) (Price, bool) {
	p, ok := c.Providers.Providers[rt.Provider]
	if !ok {
		return Price{}, false
	}
	if pr, ok := p.Prices[rt.Model]; ok {
		return pr, true
	}
	if p.Type == ProviderAnthropic {
		pr, ok := AnthropicPrices[rt.Model]
		return pr, ok
	}
	return Price{}, false
}

// Usage is the token use of one request. Prompt counts every input token,
// cached or not, as the OpenAI format does.
type Usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	CacheRead  int `json:"cache_read_tokens,omitempty"`
	CacheWrite int `json:"cache_write_tokens,omitempty"`
}

// Cost is the price of u in US dollars.
func (p Price) Cost(u Usage) float64 {
	in, out, read, write := p.Input, p.Output, p.CacheRead, p.CacheWrite
	if p.LongAbove > 0 && u.Prompt > p.LongAbove && p.Input > 0 {
		scale := p.LongInput / p.Input
		in, out, read, write = p.LongInput, p.LongOutput, read*scale, write*scale
	}
	uncached := max(u.Prompt-u.CacheRead-u.CacheWrite, 0)
	return (float64(uncached)*in + float64(u.CacheRead)*read + float64(u.CacheWrite)*write +
		float64(u.Completion)*out) / 1e6
}

// Budget limits what a client can spend at providers, in US dollars. A day
// and a month are UTC. Zero means no limit.
type Budget struct {
	DailyUSD   float64 `yaml:"daily_usd,omitempty" json:"daily_usd,omitempty"`
	MonthlyUSD float64 `yaml:"monthly_usd,omitempty" json:"monthly_usd,omitempty"`
}

func (b *Budget) set() bool { return b != nil && (b.DailyUSD > 0 || b.MonthlyUSD > 0) }

// Spend keeps the provider spend per client and per UTC day in a JSON file.
type Spend struct {
	Path string
	mu   sync.Mutex
	days map[string]map[string]float64 // day -> client -> US dollars
}

// SpendFile is the shape of usage.json.
type SpendFile struct {
	Days map[string]map[string]float64 `json:"days"`
}

// keepDays is how long the spend of a day is kept: long enough for a month.
const keepDays = 62

func (s *Spend) load() {
	if s.days != nil {
		return
	}
	s.days = map[string]map[string]float64{}
	var f SpendFile
	if b, err := os.ReadFile(s.Path); err == nil && json.Unmarshal(b, &f) == nil && f.Days != nil {
		s.days = f.Days
	}
}

// Add records usd spent by client at t.
func (s *Spend) Add(client string, usd float64, t time.Time) error {
	if s == nil || usd <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	day := t.UTC().Format(time.DateOnly)
	if s.days[day] == nil {
		s.days[day] = map[string]float64{}
	}
	s.days[day][client] += usd
	oldest := t.UTC().AddDate(0, 0, -keepDays).Format(time.DateOnly)
	for d := range s.days {
		if d < oldest {
			delete(s.days, d)
		}
	}
	return s.save()
}

func (s *Spend) save() error {
	if s.Path == "" {
		return nil
	}
	b, err := json.Marshal(SpendFile{Days: s.days})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".usage.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Spent returns what client spent on the UTC day and in the UTC month of t.
func (s *Spend) Spent(client string, t time.Time) (day, month float64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return SpentIn(s.days, client, t)
}

// SpentIn sums a client's spend for the day and the month of t.
func SpentIn(days map[string]map[string]float64, client string, t time.Time) (day, month float64) {
	today := t.UTC().Format(time.DateOnly)
	monthPrefix := today[:8] // 2006-01-
	keys := make([]string, 0, len(days))
	for d := range days {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	for _, d := range keys {
		v := days[d][client]
		if d == today {
			day += v
		}
		if len(d) >= 8 && d[:8] == monthPrefix {
			month += v
		}
	}
	return day, month
}

// overBudget returns why client cannot spend more at providers, or "".
func overBudget(b *Budget, day, month float64) string {
	switch {
	case !b.set():
		return ""
	case b.DailyUSD > 0 && day >= b.DailyUSD:
		return fmt.Sprintf("the daily budget of $%.2f is spent", b.DailyUSD)
	case b.MonthlyUSD > 0 && month >= b.MonthlyUSD:
		return fmt.Sprintf("the monthly budget of $%.2f is spent", b.MonthlyUSD)
	}
	return ""
}
