package router

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAdvise(t *testing.T) {
	h := newHarness(t, testProfiles)
	req := AdviseRequest{Provider: "cloud", Model: "big", System: "You tune llama.cpp.", Prompt: "Hardware: 2 GPUs."}
	if resp := h.post(t, "/sibyl/advise", hybridKey, req); resp.StatusCode != 401 {
		t.Errorf("a client key called advise: %d", resp.StatusCode)
	}
	for _, bad := range []AdviseRequest{{Provider: "nope", Model: "big", Prompt: "x"}, {Provider: "cloud", Model: "big"},
		{Provider: "cloud", Model: "a b", Prompt: "x"}} {
		if resp := h.post(t, "/sibyl/advise", adminKey, bad); resp.StatusCode != 400 {
			t.Errorf("%+v = %d", bad, resp.StatusCode)
		}
	}
	resp := h.post(t, "/sibyl/advise", adminKey, req)
	var ar AdviseResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil || resp.StatusCode != 200 {
		t.Fatalf("advise %d %v", resp.StatusCode, err)
	}
	if ar.Route != "cloud/big" || ar.Answer == "" {
		t.Errorf("answer %+v", ar)
	}
	got := h.provider.last()
	msgs, _ := got["messages"].([]any)
	if got["model"] != "big" || got["stream"] != false || len(msgs) != 2 {
		t.Errorf("provider got %v", got)
	}
	// The spend counts for the optimizer, not for calibration.
	if day, _ := h.router.Spend.Spent(CalibrationSpender, time.Now()); day != 0 {
		t.Errorf("calibration spend %v", day)
	}
}
