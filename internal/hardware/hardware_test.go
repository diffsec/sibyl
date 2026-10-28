package hardware

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/gpu"
)

func TestBackendsPerVariant(t *testing.T) {
	one := []gpu.GPU{{Index: 0}}
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
		if err := c2.Validate(c.v, one); (err == nil) != c.ok {
			t.Errorf("%s with %s: err %v, want ok=%v", c.v, c.backend, err, c.ok)
		}
	}
	if Default(VariantNVIDIA, nil).Backend != BackendCUDA || Default(VariantIntel, nil).Backend != BackendVulkan || Default("", nil).Backend != BackendHIP {
		t.Error("defaults per variant")
	}
}

func TestHIPNeedsASupportedTarget(t *testing.T) {
	gpus := []gpu.GPU{{Index: 0, Arch: "gfx1100"}, {Index: 1, Arch: "gfx1032"}, {Index: 2}}
	for _, c := range []struct {
		backend Backend
		gpus    []int
		ok      bool
	}{
		{BackendHIP, []int{0}, true},
		{BackendHIP, []int{0, 1}, false},
		{BackendHIP, []int{2}, true}, // unknown target: allowed
		{BackendVulkan, []int{0, 1}, true},
	} {
		err := Config{Backend: c.backend, GPUs: c.gpus, Layout: LayoutSpan}.Validate(VariantAMD, gpus)
		if (err == nil) != c.ok {
			t.Errorf("%s on %v: err %v, want ok=%v", c.backend, c.gpus, err, c.ok)
		}
	}
	if err := (Config{Backend: BackendHIP, GPUs: []int{1}, Layout: LayoutSpan}).Validate(VariantAMD, gpus); err == nil || !strings.Contains(err.Error(), "gfx1032") {
		t.Errorf("error should name the target: %v", err)
	}
	if b := Default(VariantAMD, gpus[1:]).Backend; b != BackendVulkan {
		t.Errorf("default on gfx1032 = %s, want vulkan", b)
	}
	if b := Default(VariantAMD, gpus).Backend; b != BackendHIP {
		t.Errorf("default on gfx1100 = %s, want hip", b)
	}
}

// TestHIPTargetsMatchImage keeps HIPTargets in step with the image build.
func TestHIPTargetsMatchImage(t *testing.T) {
	raw, err := os.ReadFile("../../containers/llama-swap/Containerfile.hip")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG GPU_TARGETS="?([^"\n]+)"?$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("no ARG GPU_TARGETS in Containerfile.hip")
	}
	got := strings.Split(string(m[1]), ";")
	if !slices.Equal(got, HIPTargets) {
		t.Errorf("Containerfile.hip builds %v, HIPTargets is %v", got, HIPTargets)
	}
}
