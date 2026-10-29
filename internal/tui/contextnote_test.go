package tui

import (
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/gpu"
	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/models"
)

func TestContextNote(t *testing.T) {
	// A 27B hybrid model: 29 GB of weights, 16 attention layers.
	m := client.Model{Model: models.Model{ID: "m", Size: 29 << 30}, Meta: &models.Meta{
		Architecture: "qwen35", ContextLength: 262144, KElems: 16 * 4 * 256, VElems: 16 * 4 * 256, MaxLayerKV: 2048,
	}}
	hw := hardware.Config{Backend: hardware.BackendHIP, GPUs: []int{0, 1}, Layout: hardware.LayoutSpan}
	gpus := []gpu.GPU{{Index: 0, VRAMTotal: 24 << 30}, {Index: 1, VRAMTotal: 24 << 30}}

	// Empty context, quality: the native 262,144 tokens at f16 need 18 GiB,
	// which with 29 GiB of weights exceeds 92% of 48 GiB, so Sibyl picks
	// the largest multiple of 4096 that fits at f16.
	got := contextNote(m, &settingsForm{}, hw, gpus)
	for _, want := range []string{"Empty: Sibyl uses 217,088 tokens with an f16 KV cache", "native 262,144", "of 48.0 GiB VRAM on 2 GPU(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("empty context: %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "more than the GPUs hold") {
		t.Errorf("the automatic context does not fit: %q", got)
	}

	// Empty context, context mode: a q8_0 cache keeps the native context.
	got = contextNote(m, &settingsForm{Fit: "context"}, hw, gpus)
	if !strings.Contains(got, "Empty: Sibyl uses 262,144 tokens with a q8_0 KV cache") || strings.Contains(got, "more than the GPUs hold") {
		t.Errorf("context mode: %q", got)
	}

	// The owner's native context at f16 does not fit.
	got = contextNote(m, &settingsForm{CtxSize: "262144"}, hw, gpus)
	if !strings.Contains(got, "18.0 GiB") || !strings.Contains(got, "more than the GPUs hold") {
		t.Errorf("native at f16: %q", got)
	}

	// 32k with a q8_0 cache fits.
	got = contextNote(m, &settingsForm{CtxSize: "32768", KVTypeK: "q8_0", KVTypeV: "q8_0"}, hw, gpus)
	if strings.Contains(got, "more than the GPUs hold") || !strings.Contains(got, "At 32,768 tokens") {
		t.Errorf("32k q8_0: %q", got)
	}

	// Pinned to one GPU, only that GPU's VRAM counts.
	got = contextNote(m, &settingsForm{CtxSize: "8192", Layout: "gpu:1"}, hw, gpus)
	if !strings.Contains(got, "of 24.0 GiB VRAM on 1 GPU(s)") || !strings.Contains(got, "more than the GPUs hold") {
		t.Errorf("pinned: %q", got)
	}

	// Over the native context.
	if got := contextNote(m, &settingsForm{CtxSize: "524288"}, hw, gpus); !strings.Contains(got, "over the native context") {
		t.Errorf("over native: %q", got)
	}

	// No metadata.
	if got := contextNote(client.Model{}, &settingsForm{}, hw, gpus); !strings.Contains(got, "unknown") {
		t.Errorf("no meta: %q", got)
	}
}

func TestModelGPUsFollowsPlacement(t *testing.T) {
	gpus := []gpu.GPU{{Index: 0}, {Index: 1}, {Index: 2}}
	hw := hardware.Config{GPUs: []int{2, 0}, Layout: hardware.LayoutSpan}
	idx := func(g []gpu.GPU) (out []int) {
		for _, x := range g {
			out = append(out, x.Index)
		}
		return out
	}
	for _, c := range []struct {
		layout    string
		embedding bool
		hw        hardware.Config
		want      []int
	}{
		{"", false, hw, []int{0, 2}},
		{"gpu:1", false, hw, []int{2}},
		{"", true, hw, []int{2}},
		{"", false, hardware.Config{GPUs: []int{2, 0}, Layout: hardware.LayoutPin}, []int{0}},
	} {
		if got := idx(hardware.ModelGPUs(c.layout, c.embedding, c.hw, gpus)); len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("layout %q embedding %v: %v, want %v", c.layout, c.embedding, got, c.want)
		}
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 262144: "262,144", 1048576: "1,048,576"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q", n, got)
		}
	}
}
