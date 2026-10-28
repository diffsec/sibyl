package hardware

import "testing"

func TestBackendsPerVariant(t *testing.T) {
	for _, c := range []struct {
		v       Variant
		backend Backend
		ok      bool
	}{
		{VariantAMD, BackendHIP, true},
		{VariantAMD, BackendVulkan, true},
		{VariantAMD, BackendCUDA, false},
		{VariantNVIDIA, BackendCUDA, true},
		{VariantNVIDIA, BackendVulkan, true},
		{VariantNVIDIA, BackendHIP, false},
		{VariantIntel, BackendVulkan, true},
		{VariantIntel, BackendHIP, false},
	} {
		c2 := Config{Backend: c.backend, GPUs: []int{0}, Layout: LayoutSpan}
		if err := c2.Validate(c.v, 1); (err == nil) != c.ok {
			t.Errorf("%s with %s: err %v, want ok=%v", c.v, c.backend, err, c.ok)
		}
	}
	if Default(VariantNVIDIA).Backend != BackendCUDA || Default(VariantIntel).Backend != BackendVulkan || Default("").Backend != BackendHIP {
		t.Error("defaults per variant")
	}
}
