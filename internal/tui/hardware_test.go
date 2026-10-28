package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TestHardwareShowsEveryGPU guards against huh's MultiSelect sizing, which
// drew no options for two GPUs when the field had no explicit height.
func TestHardwareShowsEveryGPU(t *testing.T) {
	gpus := []gpu.GPU{
		{Index: 0, PCIAddr: "0000:07:00.0", Vendor: "amd", VRAMTotal: 24 << 30},
		{Index: 1, PCIAddr: "0000:0b:00.0", Vendor: "amd", VRAMTotal: 24 << 30},
	}
	hw := hardware.Config{Backend: hardware.BackendHIP, GPUs: []int{0}, Layout: hardware.LayoutSpan}

	v := newHardwareView(nil)
	v.update(hardwareLoaded{hw: hw, gpus: gpus})
	summary := ansi.ReplaceAllString(v.render(120), "")
	for _, want := range []string{"GPU 0  0000:07:00.0  amd  24.0 GiB  used for inference", "GPU 1  0000:0b:00.0  amd  24.0 GiB  not used"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}

	v.update(hardwareEdit{hardwareLoaded{hw: hw, gpus: gpus}})
	form := ansi.ReplaceAllString(v.render(120), "")
	for _, want := range []string{"GPU 0  0000:07:00.0", "GPU 1  0000:0b:00.0"} {
		if !strings.Contains(form, want) {
			t.Errorf("form lacks %q:\n%s", want, form)
		}
	}
}

func TestBackendOptionsFollowTheVariant(t *testing.T) {
	v := newHardwareView(nil)
	gpus := []gpu.GPU{{Index: 0, PCIAddr: "0000:01:00.0", Vendor: "nvidia"}}
	v.update(hardwareEdit{hardwareLoaded{hw: hardware.Default(hardware.VariantNVIDIA), gpus: gpus,
		backends: hardware.Backends(hardware.VariantNVIDIA)}})
	form := ansi.ReplaceAllString(v.render(120), "")
	if !strings.Contains(form, "CUDA") || strings.Contains(form, "HIP") {
		t.Fatalf("NVIDIA form backends:\n%s", form)
	}
}
