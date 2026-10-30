package modelform

import (
	"reflect"
	"testing"

	"github.com/diffsec/sibyl/internal/models"
)

func TestSettingsFormRoundTrip(t *testing.T) {
	temp, topP, topK, draftMax := 0.6, 0.95, 20, 3
	in := models.Settings{
		Layout:          "gpu:1",
		CtxSize:         262144,
		Sampling:        models.Sampling{Temperature: &temp, TopP: &topP, TopK: &topK},
		Speculative:     models.Speculative{Types: []string{"draft-simple"}, DraftModel: "drafter", DraftMax: &draftMax},
		KVCache:         models.KVCache{TypeK: "q8_0", TypeV: "q8_0"},
		MoE:             models.MoE{CPULayers: models.AllLayers, CacheMiB: 8192},
		Args:            []string{"--batch-size=2048"},
		ReasoningEffort: "high",
	}
	out, err := FromSettings(in).Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip changed settings:\n in  %+v\n out %+v", in, out)
	}
}

func TestSettingsFormEmptyAndErrors(t *testing.T) {
	out, err := Form{}.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if out.Sampling.Temperature != nil || out.CtxSize != 0 || out.Speculative.Types != nil {
		t.Errorf("empty form = %+v", out)
	}
	if _, err := (Form{Temperature: "warm"}).Settings(); err == nil {
		t.Error("want an error for a non-numeric temperature")
	}
	if _, err := (Form{TopK: "2.5"}).Settings(); err == nil {
		t.Error("want an error for a fractional top k")
	}
	if out, err := (Form{MoELayers: " 12 "}).Settings(); err != nil || out.MoE.CPULayers != 12 {
		t.Errorf("moe layers 12 = %+v, %v", out.MoE, err)
	}
	if _, err := (Form{MoELayers: "most"}).Settings(); err == nil {
		t.Error("want an error for experts in system RAM that is not all or a number")
	}
	// A draft model without a speculative type is dropped, and "none" is not a type.
	out, _ = Form{SpecTypes: []string{"none"}, DraftModel: "x"}.Settings()
	if out.Speculative.Types != nil || out.Speculative.DraftModel != "" {
		t.Errorf("speculative = %+v", out.Speculative)
	}
}
