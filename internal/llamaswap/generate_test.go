package llamaswap

import (
	"os"
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/models"
)

var installed = []models.Model{
	{ID: "big-70b", Path: "big-70b-Q4_K_M-00001-of-00002.gguf"},
	{ID: "chat-14b", Path: "chat-14b-Q8_0.gguf"},
	{ID: "coder-32b", Path: "coder/coder-32b-Q4_K_M.gguf"},
	{ID: "embed", Path: "embed-f16.gguf"},
	{ID: "bad path", Path: "has space.gguf"},
}

func twoGPUs(layout hardware.Layout) hardware.Config {
	return hardware.Config{Backend: hardware.BackendHIP, GPUs: []int{1, 0}, Layout: layout}
}

func TestGenerateSpanDefault(t *testing.T) {
	cfg, issues := Generate(twoGPUs(hardware.LayoutSpan), installed, map[string]models.Settings{
		"chat-14b":  {Layout: "gpu:0", CtxSize: 32768},
		"coder-32b": {Layout: "gpu:1"},
		"embed":     {Embedding: true},
	})

	if len(issues) != 1 || issues[0].ID != "bad path" || !issues[0].Skipped {
		t.Fatalf("issues = %+v, want only the model with a space in its path, skipped", issues)
	}
	big := cfg.Models["big-70b"].Cmd
	if !strings.Contains(big, "--split-mode layer --device ROCm0,ROCm1") {
		t.Errorf("spanning model cmd = %q", big)
	}
	if !strings.Contains(big, "--model /models/big-70b-Q4_K_M-00001-of-00002.gguf") {
		t.Errorf("spanning model path wrong: %q", big)
	}
	chat := cfg.Models["chat-14b"].Cmd
	if !strings.Contains(chat, "--split-mode none --device ROCm0") || !strings.Contains(chat, "--ctx-size 32768") {
		t.Errorf("pinned model cmd = %q", chat)
	}
	emb := cfg.Models["embed"].Cmd
	if !strings.Contains(emb, "--embedding") || strings.Contains(emb, "--jinja") || !strings.Contains(emb, "--device ROCm1") {
		t.Errorf("embedding model cmd = %q", emb)
	}

	m := cfg.Routing.Router.Settings.Matrix
	expand := func(expr string) string {
		for v, id := range m.Vars {
			expr = strings.ReplaceAll(expr, v+" ", id+" ")
			expr = strings.ReplaceAll(expr, v+")", id+")")
			if strings.HasSuffix(expr, v) {
				expr = strings.TrimSuffix(expr, v) + id
			}
		}
		return expr
	}
	if got := expand(m.Sets["pinned"]); got != "(chat-14b) & (coder-32b) & embed" {
		t.Errorf("pinned set = %q", got)
	}
	if got := expand(m.Sets["span-0"]); got != "big-70b & embed" {
		t.Errorf("span set = %q", got)
	}
	for v, id := range m.Vars {
		if id == "embed" && m.EvictCosts[v] != embeddingEvictCost {
			t.Errorf("embedding evict cost = %d", m.EvictCosts[v])
		}
	}
}

func TestGeneratePinDefaultAndVulkan(t *testing.T) {
	hw := twoGPUs(hardware.LayoutPin)
	hw.Backend = hardware.BackendVulkan
	cfg, _ := Generate(hw, installed[:3], nil)
	for id, e := range cfg.Models {
		if !strings.Contains(e.Cmd, "--split-mode none --device Vulkan0") {
			t.Errorf("%s: want pinned to Vulkan0 by default, cmd = %q", id, e.Cmd)
		}
	}
	if s := cfg.Routing.Router.Settings.Matrix.Sets; len(s) != 1 || s["pinned"] == "" {
		t.Errorf("sets = %+v", s)
	}
}

func TestGenerateSingleGPUNeverSplits(t *testing.T) {
	hw := hardware.Config{Backend: hardware.BackendHIP, GPUs: []int{0}, Layout: hardware.LayoutSpan}
	cfg, _ := Generate(hw, installed[:1], nil)
	if cmd := cfg.Models["big-70b"].Cmd; !strings.Contains(cmd, "--split-mode none --device ROCm0") {
		t.Errorf("cmd = %q", cmd)
	}
}

func TestGenerateNoModels(t *testing.T) {
	cfg, _ := Generate(hardware.Default(hardware.VariantAMD, nil), nil, nil)
	if cfg.Routing != nil || len(cfg.Models) != 0 {
		t.Errorf("want an empty config, got %+v", cfg)
	}
}

func TestDropIn(t *testing.T) {
	got := DropIn(hardware.VariantAMD, twoGPUs(hardware.LayoutSpan), "ghcr.io/ihavespoons", "1.2.3")
	for _, want := range []string{
		"Image=ghcr.io/ihavespoons/sibyl-llama-swap:1.2.3-hip\n",
		"Environment=HIP_VISIBLE_DEVICES=0,1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in missing %q:\n%s", want, got)
		}
	}
	hw := hardware.Config{Backend: hardware.BackendVulkan, GPUs: []int{1}}
	if got := DropIn(hardware.VariantAMD, hw, "r", "t"); !strings.Contains(got, "Environment=GGML_VK_VISIBLE_DEVICES=1\n") {
		t.Errorf("vulkan drop-in:\n%s", got)
	}
}

// The image ships a default backend.conf. It must match the drop-in for the
// default hardware byte for byte, or the first apply restarts llama-swap.
// TestImageDefaultDropIn checks that the image's default drop-in for each
// variant matches DropIn(Default), so a first boot with no hardware.yaml
// does not restart llama-swap.
func TestImageDefaultDropIn(t *testing.T) {
	for _, v := range []hardware.Variant{hardware.VariantAMD, hardware.VariantNVIDIA, hardware.VariantIntel} {
		raw, err := os.ReadFile("../../os/rootfs/usr/share/sibyl/backend/" + string(v) + ".conf")
		if err != nil {
			t.Fatal(err)
		}
		got := strings.ReplaceAll(string(raw), "@IMAGE_TAG@", "tag")
		want := DropIn(v, hardware.Default(v, nil), "ghcr.io/ihavespoons", "tag")
		if got != want {
			t.Errorf("%s: image default drop-in differs from DropIn(Default()):\n--- image\n%s--- generated\n%s", v, got, want)
		}
	}
}

func TestDropInNVIDIA(t *testing.T) {
	hw := hardware.Config{Backend: hardware.BackendCUDA, GPUs: []int{1, 0}, Layout: hardware.LayoutSpan}
	got := DropIn(hardware.VariantNVIDIA, hw, "r", "t")
	for _, want := range []string{"Image=r/sibyl-llama-swap:t-cuda\n", "AddDevice=nvidia.com/gpu=all\n", "Environment=CUDA_VISIBLE_DEVICES=0,1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/dev/kfd") {
		t.Error("an NVIDIA drop-in adds /dev/kfd")
	}
	if DevicePrefix(hardware.BackendCUDA) != "CUDA" {
		t.Error("CUDA device prefix")
	}
}

func TestGenerateTuning(t *testing.T) {
	temp, draftMax := 0.6, 3
	ms := []models.Model{{ID: "big", Path: "big.gguf"}, {ID: "drafter", Path: "d/drafter.gguf"}}
	cfg, issues := Generate(twoGPUs(hardware.LayoutSpan), ms, map[string]models.Settings{
		"big": {
			CtxSize:     262144,
			Sampling:    models.Sampling{Temperature: &temp},
			Speculative: models.Speculative{Types: []string{"draft-dflash"}, DraftModel: "drafter", DraftMax: &draftMax},
			KVCache:     models.KVCache{TypeK: "q8_0", TypeV: "q8_0"},
		},
	})
	if len(issues) != 0 {
		t.Fatalf("issues = %+v", issues)
	}
	cmd := cfg.Models["big"].Cmd
	for _, want := range []string{
		"--cache-type-k q8_0 --cache-type-v q8_0",
		"--temp 0.6",
		"--spec-type draft-dflash --spec-draft-model /models/d/drafter.gguf --spec-draft-n-max 3",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("cmd missing %q:\n%s", want, cmd)
		}
	}
}

func TestGenerateMissingDraftModel(t *testing.T) {
	ms := []models.Model{{ID: "big", Path: "big.gguf"}}
	cfg, issues := Generate(twoGPUs(hardware.LayoutSpan), ms, map[string]models.Settings{
		"big": {Speculative: models.Speculative{Types: []string{"draft-simple"}, DraftModel: "gone"}},
	})
	if len(issues) != 1 || issues[0].Skipped {
		t.Fatalf("issues = %+v, want one non-skipping issue", issues)
	}
	if strings.Contains(cfg.Models["big"].Cmd, "--spec-type") {
		t.Errorf("speculative flags emitted with a missing draft model: %s", cfg.Models["big"].Cmd)
	}
}
