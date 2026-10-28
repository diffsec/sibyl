package tui

import (
	"reflect"
	"testing"

	"github.com/diffsec/sibyl/internal/models"
)

func TestSettingsFormRoundTrip(t *testing.T) {
	temp, topP, topK, draftMax := 0.6, 0.95, 20, 3
	in := models.Settings{
		Layout:      "gpu:1",
		CtxSize:     262144,
		Sampling:    models.Sampling{Temperature: &temp, TopP: &topP, TopK: &topK},
		Speculative: models.Speculative{Types: []string{"draft-simple"}, DraftModel: "drafter", DraftMax: &draftMax},
		KVCache:     models.KVCache{TypeK: "q8_0", TypeV: "q8_0"},
		Args:        []string{"--batch-size=2048"},
	}
	out, err := formFromSettings(in).toSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip changed settings:\n in  %+v\n out %+v", in, out)
	}
}

func TestSettingsFormEmptyAndErrors(t *testing.T) {
	out, err := settingsForm{}.toSettings()
	if err != nil {
		t.Fatal(err)
	}
	if out.Sampling.Temperature != nil || out.CtxSize != 0 || out.Speculative.Types != nil {
		t.Errorf("empty form = %+v", out)
	}
	if _, err := (settingsForm{Temperature: "warm"}).toSettings(); err == nil {
		t.Error("want an error for a non-numeric temperature")
	}
	if _, err := (settingsForm{TopK: "2.5"}).toSettings(); err == nil {
		t.Error("want an error for a fractional top k")
	}
	// A draft model without a speculative type is dropped, and "none" is not a type.
	out, _ = settingsForm{SpecTypes: []string{"none"}, DraftModel: "x"}.toSettings()
	if out.Speculative.Types != nil || out.Speculative.DraftModel != "" {
		t.Errorf("speculative = %+v", out.Speculative)
	}
}

func TestWindow(t *testing.T) {
	listHeight = 5
	for _, c := range []struct{ n, cursor, start, end int }{
		{3, 0, 0, 3},
		{20, 0, 0, 5},
		{20, 10, 8, 13},
		{20, 19, 15, 20},
	} {
		if s, e := window(c.n, c.cursor); s != c.start || e != c.end {
			t.Errorf("window(%d, %d) = %d,%d, want %d,%d", c.n, c.cursor, s, e, c.start, c.end)
		}
	}
}
