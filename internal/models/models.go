// Package models lists installed GGUF models and stores per-model settings.
package models

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/diffsec/sibyl/internal/hardware"
)

// Model is one installed GGUF model. A split model is listed once, by its first shard.
type Model struct {
	ID     string `json:"id"`
	Path   string `json:"path"`           // relative to the models directory, or to its pool
	Pool   string `json:"pool,omitempty"` // empty for the models directory
	Size   int64  `json:"size_bytes"`
	Shards int    `json:"shards"`
}

// PoolRE matches the name of a model pool directory.
var PoolRE = regexp.MustCompile(`^pool-[0-9]{1,2}$`)

// Pools returns the pool directories under poolsDir, in name order. A
// missing poolsDir has no pools.
func Pools(poolsDir string) ([]string, error) {
	entries, err := os.ReadDir(poolsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && PoolRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ScanAll scans the models directory and every pool. When two places hold
// a model with the same ID, the models directory wins, then the lower pool.
func ScanAll(modelsDir, poolsDir string) ([]Model, error) {
	all, err := Scan(modelsDir)
	if err != nil {
		return nil, err
	}
	pools, err := Pools(poolsDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range all {
		seen[m.ID] = true
	}
	for _, p := range pools {
		found, err := Scan(filepath.Join(poolsDir, p))
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", p, err)
		}
		for _, m := range found {
			if !seen[m.ID] {
				m.Pool = p
				seen[m.ID] = true
				all = append(all, m)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}

// Root returns the directory that m.Path is relative to.
func (m Model) Root(modelsDir, poolsDir string) string {
	if m.Pool == "" {
		return modelsDir
	}
	return filepath.Join(poolsDir, m.Pool)
}

var (
	splitShard = regexp.MustCompile(`^(.*)-([0-9]{5})-of-([0-9]{5})\.gguf$`)
	idUnsafe   = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// Scan walks dir for *.gguf files. It skips multimodal projector files
// (mmproj-*), and lists a split model once with the size of all shards.
func Scan(dir string) ([]Model, error) {
	byID := map[string]*Model{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // partial downloads live in hidden directories
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") || strings.HasPrefix(strings.ToLower(name), "mmproj") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		base, first := strings.TrimSuffix(name, filepath.Ext(name)), true
		if m := splitShard.FindStringSubmatch(name); m != nil {
			base, first = m[1], m[2] == "00001"
		}
		id := ID(filepath.Join(filepath.Dir(rel), base))
		mod, ok := byID[id]
		if !ok {
			mod = &Model{ID: id}
			byID[id] = mod
		}
		mod.Size += info.Size()
		mod.Shards++
		if first {
			mod.Path = rel
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]Model, 0, len(byID))
	for _, m := range byID {
		if m.Path == "" {
			continue // a split model without its first shard cannot load
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ID turns a relative path into a model ID that is safe in URLs and llama-swap config.
// A file in a folder of the same name, such as qwen/Qwen.gguf, gets the short ID "qwen".
func ID(rel string) string {
	if dir, base := filepath.Split(filepath.ToSlash(rel)); dir != "" &&
		strings.EqualFold(filepath.Base(strings.TrimSuffix(dir, "/")), base) {
		rel = strings.TrimSuffix(dir, "/")
	}
	id := strings.ToLower(filepath.ToSlash(rel))
	id = strings.ReplaceAll(id, "/", "--")
	id = idUnsafe.ReplaceAllString(id, "-")
	return strings.Trim(id, "-.")
}

// Settings are the per-model choices that the generator needs.
type Settings struct {
	// Layout overrides the hardware default: "span", or "gpu:N" where N is a
	// position in the selected GPU list.
	Layout      string      `yaml:"layout,omitempty" json:"layout,omitempty"`
	CtxSize     int         `yaml:"ctx_size,omitempty" json:"ctx_size,omitempty"`
	Embedding   bool        `yaml:"embedding,omitempty" json:"embedding,omitempty"`
	Hidden      bool        `yaml:"hidden,omitempty" json:"hidden,omitempty"`
	Sampling    Sampling    `yaml:"sampling,omitempty" json:"sampling,omitzero"`
	Speculative Speculative `yaml:"speculative,omitempty" json:"speculative,omitzero"`
	KVCache     KVCache     `yaml:"kv_cache,omitempty" json:"kv_cache,omitzero"`
	MoE         MoE         `yaml:"moe,omitempty" json:"moe,omitzero"`
	// Fit decides what an automatic context gives up when VRAM is short:
	// quality (the default) or context.
	Fit FitMode `yaml:"fit,omitempty" json:"fit,omitempty"`
	// Preset is the sampling preset for the model's default use. The
	// sampling fields above override its values.
	Preset string `yaml:"preset,omitempty" json:"preset,omitempty"`
	// Parallel is the number of requests that llama-server runs at once
	// (--parallel). The slots share the context. 0 leaves llama-server's
	// default.
	Parallel int `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	// TensorSplit is the share of the layers on each GPU of a spanning
	// model (--tensor-split). Empty lets Sibyl split by the memory that
	// each GPU has left.
	TensorSplit []float64 `yaml:"tensor_split,omitempty" json:"tensor_split,omitempty"`
	// Args are extra llama-server flags for anything the typed fields do not cover.
	Args []string `yaml:"args,omitempty" json:"args,omitempty"`
}

// MaxParallel is the most parallel slots that a model can have.
const MaxParallel = 64

var (
	safeArg     = regexp.MustCompile(`^[A-Za-z0-9_.,:=/+-]+$`)
	reservedArg = map[string]bool{
		"--model": true, "-m": true, "--host": true, "--port": true,
		"--device": true, "-dev": true, "--split-mode": true, "-sm": true,
		"--hf-repo": true, "-hf": true, "--api-key": true, "--api-key-file": true,
	}
	gpuLayout = regexp.MustCompile(`^gpu:([0-9]+)$`)
)

// Validate checks the settings against the number of selected GPUs.
func (s Settings) Validate(selected int) error {
	if s.Layout != "" && s.Layout != string(hardware.LayoutSpan) {
		m := gpuLayout.FindStringSubmatch(s.Layout)
		if m == nil {
			return fmt.Errorf("layout %q: must be %q or gpu:N", s.Layout, hardware.LayoutSpan)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n >= selected {
			return fmt.Errorf("layout %q: only %d GPUs are selected", s.Layout, selected)
		}
	}
	if s.CtxSize < 0 {
		return errors.New("ctx_size: must not be negative")
	}
	if s.Parallel < 0 || s.Parallel > MaxParallel {
		return fmt.Errorf("parallel: must be 0 to %d", MaxParallel)
	}
	if len(s.TensorSplit) > selected {
		return fmt.Errorf("tensor_split: %d values for %d selected GPUs", len(s.TensorSplit), selected)
	}
	for _, v := range s.TensorSplit {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("tensor_split: values must be 0 or more")
		}
	}
	if err := s.Sampling.Validate(); err != nil {
		return err
	}
	if err := s.Speculative.Validate(); err != nil {
		return err
	}
	if err := s.KVCache.Validate(); err != nil {
		return err
	}
	if err := s.MoE.Validate(); err != nil {
		return err
	}
	if s.Fit != "" && s.Fit != FitQuality && s.Fit != FitContext {
		return fmt.Errorf("fit %q: must be %s or %s", s.Fit, FitQuality, FitContext)
	}
	if s.Preset != "" && !slices.Contains(Presets, s.Preset) {
		return fmt.Errorf("preset %q: use one of %s", s.Preset, strings.Join(Presets, ", "))
	}
	for _, a := range s.Args {
		if !safeArg.MatchString(a) {
			return fmt.Errorf("args: %q contains characters that are not allowed", a)
		}
		flag, _, _ := strings.Cut(a, "=")
		if reservedArg[flag] {
			return fmt.Errorf("args: %q is set by Sibyl and cannot be overridden", flag)
		}
		if slices.Contains(typedFlags, flag) {
			return fmt.Errorf("args: %q has a typed setting; set it there instead", flag)
		}
	}
	return nil
}

// LoadSettings reads models.yaml. A missing file returns an empty map.
func LoadSettings(path string) (map[string]Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Settings{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Settings{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// SaveSettings writes models.yaml atomically.
func SaveSettings(path string, s map[string]Settings) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return hardware.WriteFileAtomic(path, data, 0o640)
}
