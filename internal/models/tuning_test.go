package models

import (
	"slices"
	"testing"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

func TestTuningValidate(t *testing.T) {
	cases := []struct {
		name    string
		s       Settings
		wantErr bool
	}{
		{"sampling ok", Settings{Sampling: Sampling{Temperature: f(0.7), TopK: i(20), TopP: f(0.95), MinP: f(0)}}, false},
		{"temperature too high", Settings{Sampling: Sampling{Temperature: f(9)}}, true},
		{"top_p above 1", Settings{Sampling: Sampling{TopP: f(1.5)}}, true},
		{"presence penalty negative ok", Settings{Sampling: Sampling{PresencePenalty: f(-1)}}, false},
		{"ngram-mod needs no draft", Settings{Speculative: Speculative{Types: []string{"ngram-mod"}, NgramModMin: i(4)}}, false},
		{"draft-mtp needs no draft", Settings{Speculative: Speculative{Types: []string{"draft-mtp"}}}, false},
		{"draft-dflash needs draft", Settings{Speculative: Speculative{Types: []string{"draft-dflash"}}}, true},
		{"draft model without a draft type", Settings{Speculative: Speculative{Types: []string{"ngram-mod"}, DraftModel: "x"}}, true},
		{"unknown spec type", Settings{Speculative: Speculative{Types: []string{"eagle9"}}}, true},
		{"draft_min above draft_max", Settings{Speculative: Speculative{Types: []string{"ngram-mod"}, DraftMax: i(4), DraftMin: i(8)}}, true},
		{"kv q8_0", Settings{KVCache: KVCache{TypeK: "q8_0", TypeV: "q8_0"}}, false},
		{"kv unknown", Settings{KVCache: KVCache{TypeK: "q3_x"}}, true},
		{"raw temp blocked", Settings{Args: []string{"--temp", "0.5"}}, true},
		{"raw spec-type blocked", Settings{Args: []string{"--spec-type=ngram-mod"}}, true},
		{"raw ctx blocked", Settings{Args: []string{"-c", "4096"}}, true},
	}
	for _, c := range cases {
		if err := c.s.Validate(2); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestTuningFlags(t *testing.T) {
	got := Sampling{Temperature: f(0.6), TopK: i(20), MinP: f(0)}.Flags()
	want := []string{"--temp", "0.6", "--top-k", "20", "--min-p", "0"}
	if !slices.Equal(got, want) {
		t.Errorf("sampling flags = %v, want %v", got, want)
	}
	got = Speculative{Types: []string{"draft-dflash", "ngram-mod"}, DraftMax: i(3)}.Flags("/models/d.gguf")
	want = []string{"--spec-type", "draft-dflash,ngram-mod", "--spec-draft-model", "/models/d.gguf", "--spec-draft-n-max", "3"}
	if !slices.Equal(got, want) {
		t.Errorf("speculative flags = %v, want %v", got, want)
	}
	if got := (Speculative{}).Flags(""); got != nil {
		t.Errorf("empty speculative flags = %v", got)
	}
	got = KVCache{TypeK: "q8_0", TypeV: "q8_0"}.Flags()
	want = []string{"--cache-type-k", "q8_0", "--cache-type-v", "q8_0"}
	if !slices.Equal(got, want) {
		t.Errorf("kv flags = %v, want %v", got, want)
	}
}
