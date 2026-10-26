// Package llamaswap generates the llama-swap configuration and the Quadlet
// drop-in that selects the inference image and its GPUs.
package llamaswap

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/models"
)

// ServerBinary is the llama-server path inside the inference image.
const ServerBinary = "/opt/llama/bin/llama-server"

// ModelsMount is where the models directory is mounted in the inference container.
const ModelsMount = "/models"

// embeddingEvictCost keeps embedding models loaded when llama-swap must evict.
const embeddingEvictCost = 100

// Config is the subset of the llama-swap v266 configuration that Sibyl writes.
type Config struct {
	HealthCheckTimeout int                   `yaml:"healthCheckTimeout"`
	LogLevel           string                `yaml:"logLevel"`
	StartPort          int                   `yaml:"startPort"`
	SendLoadingState   bool                  `yaml:"sendLoadingState"`
	Routing            *Routing              `yaml:"routing,omitempty"`
	Models             map[string]ModelEntry `yaml:"models"`
}

type Routing struct {
	Router Router `yaml:"router"`
}

type Router struct {
	Use      string         `yaml:"use"`
	Settings RouterSettings `yaml:"settings"`
}

type RouterSettings struct {
	Matrix Matrix `yaml:"matrix"`
}

type Matrix struct {
	Vars       map[string]string `yaml:"vars"`
	EvictCosts map[string]int    `yaml:"evict_costs,omitempty"`
	Sets       map[string]string `yaml:"sets"`
}

type ModelEntry struct {
	Name     string `yaml:"name"`
	Cmd      string `yaml:"cmd"`
	Unlisted bool   `yaml:"unlisted,omitempty"`
}

// Skipped records a model that the generator left out, and why.
type Skipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// placement is where one model runs: on every selected GPU, or on one.
type placement struct {
	span bool
	gpu  int // position in the selected GPU list, when span is false
}

// Generate builds the llama-swap configuration for the installed models.
func Generate(hw hardware.Config, installed []models.Model, settings map[string]models.Settings) (Config, []Skipped) {
	cfg := Config{
		HealthCheckTimeout: 600,
		LogLevel:           "info",
		StartPort:          10001,
		SendLoadingState:   true,
		Models:             map[string]ModelEntry{},
	}
	selected := len(hw.GPUs)
	devPrefix := DevicePrefix(hw.Backend)

	var skipped []Skipped
	perGPU := make([][]string, selected) // chat models pinned to each GPU
	var spanning, embedding []string

	for _, m := range installed {
		s := settings[m.ID]
		if err := s.Validate(selected); err != nil {
			skipped = append(skipped, Skipped{ID: m.ID, Reason: err.Error()})
			continue
		}
		if strings.ContainsAny(m.Path, " \t\n\"'\\") {
			skipped = append(skipped, Skipped{ID: m.ID, Reason: "file path contains spaces, quotes or backslashes"})
			continue
		}
		p := place(hw, s, selected)
		cfg.Models[m.ID] = ModelEntry{
			Name:     m.ID,
			Cmd:      command(m, s, p, selected, devPrefix),
			Unlisted: s.Hidden,
		}
		switch {
		case s.Embedding:
			embedding = append(embedding, m.ID)
		case p.span && selected > 1:
			spanning = append(spanning, m.ID)
		default:
			perGPU[p.gpu] = append(perGPU[p.gpu], m.ID)
		}
	}

	cfg.Routing = matrix(perGPU, spanning, embedding)
	return cfg, skipped
}

// place picks the placement for a model. Embedding models default to the
// last selected GPU, so that they stay out of the way of chat models on GPU 0.
func place(hw hardware.Config, s models.Settings, selected int) placement {
	switch {
	case s.Layout == string(hardware.LayoutSpan):
		return placement{span: true}
	case strings.HasPrefix(s.Layout, "gpu:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(s.Layout, "gpu:"))
		return placement{gpu: n}
	case s.Embedding:
		return placement{gpu: selected - 1}
	case hw.Layout == hardware.LayoutPin:
		return placement{gpu: 0}
	default:
		return placement{span: true}
	}
}

func command(m models.Model, s models.Settings, p placement, selected int, devPrefix string) string {
	args := []string{
		ServerBinary,
		"--host", "127.0.0.1",
		"--port", "${PORT}",
		"--model", path.Join(ModelsMount, strings.ReplaceAll(m.Path, "\\", "/")),
		"--n-gpu-layers", "all",
		"--flash-attn", "on",
		"--metrics",
	}
	if s.Embedding {
		args = append(args, "--embedding")
	} else {
		args = append(args, "--jinja")
	}
	if s.CtxSize > 0 {
		args = append(args, "--ctx-size", strconv.Itoa(s.CtxSize))
	}
	if p.span && selected > 1 {
		devs := make([]string, selected)
		for i := range devs {
			devs[i] = devPrefix + strconv.Itoa(i)
		}
		args = append(args, "--split-mode", "layer", "--device", strings.Join(devs, ","))
	} else {
		args = append(args, "--split-mode", "none", "--device", devPrefix+strconv.Itoa(p.gpu))
	}
	args = append(args, s.Args...)
	return strings.Join(args, " ")
}

// matrix lists which models can run together:
//   - one chat model per GPU, plus every embedding model;
//   - or one spanning model, plus every embedding model.
func matrix(perGPU [][]string, spanning, embedding []string) *Routing {
	vars := map[string]string{}
	varOf := map[string]string{}
	name := func(id string) string {
		if v, ok := varOf[id]; ok {
			return v
		}
		v := "m" + strconv.Itoa(len(vars))
		vars[v] = id
		varOf[id] = v
		return v
	}
	var embedTerms []string
	for _, id := range embedding {
		embedTerms = append(embedTerms, name(id))
	}
	withEmbed := func(expr string) string {
		if len(embedTerms) == 0 {
			return expr
		}
		if expr == "" {
			return strings.Join(embedTerms, " & ")
		}
		return expr + " & " + strings.Join(embedTerms, " & ")
	}

	sets := map[string]string{}
	var perGPUTerms []string
	for _, ids := range perGPU {
		if len(ids) == 0 {
			continue
		}
		alts := make([]string, len(ids))
		for i, id := range ids {
			alts[i] = name(id)
		}
		perGPUTerms = append(perGPUTerms, "("+strings.Join(alts, " | ")+")")
	}
	if len(perGPUTerms) > 0 || len(embedTerms) > 0 {
		sets["pinned"] = withEmbed(strings.Join(perGPUTerms, " & "))
	}
	for i, id := range spanning {
		sets["span-"+strconv.Itoa(i)] = withEmbed(name(id))
	}
	if len(sets) == 0 {
		return nil
	}

	costs := map[string]int{}
	for _, v := range embedTerms {
		costs[v] = embeddingEvictCost
	}
	return &Routing{Router: Router{
		Use:      "matrix",
		Settings: RouterSettings{Matrix: Matrix{Vars: vars, EvictCosts: costs, Sets: sets}},
	}}
}

// DevicePrefix is the llama.cpp device name prefix for a backend.
func DevicePrefix(b hardware.Backend) string {
	if b == hardware.BackendVulkan {
		return "Vulkan"
	}
	return "ROCm"
}

// Marshal renders the configuration as YAML with a header comment.
func Marshal(cfg Config) ([]byte, error) {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	header := "# Generated by sibyl-api. Edits are overwritten on the next apply.\n" +
		"# Change hardware.yaml or models.yaml instead.\n"
	return append([]byte(header), body...), nil
}

// DropIn renders the Quadlet drop-in for llama-swap.container. It selects the
// image for the backend and limits the container to the selected GPUs.
//
// The GPU indexes follow PCI order (see gpu.Detect). ROCm and Vulkan
// renumber the visible devices from 0, which is why command() uses positions.
func DropIn(hw hardware.Config, registry, imageTag string) string {
	idx := make([]string, len(hw.GPUs))
	sorted := append([]int(nil), hw.GPUs...)
	sort.Ints(sorted)
	for i, g := range sorted {
		idx[i] = strconv.Itoa(g)
	}
	visible := "HIP_VISIBLE_DEVICES"
	if hw.Backend == hardware.BackendVulkan {
		visible = "GGML_VK_VISIBLE_DEVICES"
	}
	return fmt.Sprintf(`# Generated by sibyl-api from hardware.yaml.
[Container]
Image=%s/sibyl-llama-swap:%s-%s
Environment=%s=%s
`, registry, imageTag, hw.Backend, visible, strings.Join(idx, ","))
}
