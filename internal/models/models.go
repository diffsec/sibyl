// Package models lists installed models and stores per-model settings.
package models

import (
	"encoding/json"
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

// EngineEdge0 is the Engine of an Edge0 model folder, which edge0-strata
// runs. A GGUF model has an empty Engine, and llama.cpp runs it.
const EngineEdge0 = "edge0"

// Model is one installed model: a GGUF model, listed once by its first
// shard, or an Edge0 model folder.
type Model struct {
	ID     string `json:"id"`
	Path   string `json:"path"` // relative to the models directory, or to its pool
	Engine string `json:"engine,omitempty"`
	// Context is the context length that an Edge0 folder declares. A GGUF
	// model's context is in its file metadata.
	Context int `json:"context,omitempty"`
	// RAM is the system memory that an Edge0 model keeps while it runs.
	// A GGUF model runs from VRAM, and it has no value here.
	RAM    int64  `json:"ram_bytes,omitempty"`
	Pool   string `json:"pool,omitempty"` // empty for the models directory
	Size   int64  `json:"size_bytes"`
	Shards int    `json:"shards"`
	// Drafter is true for a draft model that cannot answer requests by
	// itself, such as a DFlash or EAGLE-3 drafter. The scan does not set
	// it: the System API reads it from the file's architecture.
	Drafter bool `json:"drafter,omitempty"`
}

// Edge0DefaultContext is the context that an Edge0 model runs with when its
// settings name none. It is the value that the model's own launch
// configuration uses, and the value the engine was measured with.
const Edge0DefaultContext = 131072

// Edge0RAMOverhead is the system memory that the Edge0 engine keeps beside
// the expert file. On the test host, Edge0 145B with a 37.3 GiB expert file
// took 41 GiB of RAM.
const Edge0RAMOverhead = 6 << 30

// Edge0RAM returns the system memory that an Edge0 model keeps while it
// runs, for the size of its expert file. The engine holds the whole file in
// RAM and copies experts from there to the GPUs.
func Edge0RAM(experts int64) int64 { return experts + Edge0RAMOverhead }

// Edge0Context returns the context that an Edge0 model runs with for the
// ctx_size setting: the setting, or the default, and never more than the
// folder declares.
func (m Model) Edge0Context(ctxSize int) int {
	ctx := ctxSize
	if ctx <= 0 {
		ctx = Edge0DefaultContext
	}
	if m.Context > 0 && ctx > m.Context {
		ctx = m.Context
	}
	return ctx
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
			if path == dir {
				return nil
			}
			ctx, experts, ok := edge0Folder(path)
			if !ok {
				return nil
			}
			// The folder is one model. Its GGUF files are parts of it.
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			size, err := dirSize(path)
			if err != nil {
				return err
			}
			id := ID(rel)
			byID[id] = &Model{ID: id, Path: rel, Engine: EngineEdge0, Context: ctx, RAM: Edge0RAM(experts), Size: size, Shards: 1}
			return filepath.SkipDir
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

// edge0Manifest is the part of an Edge0 folder's model.json that Sibyl reads.
type edge0Manifest struct {
	Family  string `json:"family"`
	Context int    `json:"context"`
}

// edge0Folder reports whether dir is an Edge0 model folder. It returns the
// context length that the folder's model.json declares, and the size of the
// expert file. The folder holds a strata pack: the expert file, and a
// model.json that names the Edge0 family.
func edge0Folder(dir string) (ctx int, experts int64, ok bool) {
	path := filepath.Join(dir, "model.json")
	// The manifest is about 2 KB. A large file is not one.
	if fi, err := os.Stat(path); err != nil || fi.Size() > 1<<20 {
		return 0, 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	var m edge0Manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Family != "Edge0" {
		return 0, 0, false
	}
	fi, err := os.Stat(filepath.Join(dir, "experts.bin"))
	if err != nil {
		return 0, 0, false
	}
	return m.Context, fi.Size(), true
}

// dirSize returns the total size of the regular files under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
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
	// Reasoning is the default reasoning, in the variable that the model's
	// chat template reads. Nil leaves the template's default.
	Reasoning *Reasoning `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
	// Parallel is the number of requests that llama-server runs at once
	// (--parallel). The slots share the context. 0 leaves llama-server's
	// default.
	Parallel int `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	// TensorSplit is the share of the layers on each GPU of a spanning
	// model (--tensor-split). Empty lets Sibyl split by the memory that
	// each GPU has left.
	TensorSplit []float64 `yaml:"tensor_split,omitempty" json:"tensor_split,omitempty"`
	// Batch is the logical batch size (--batch-size), and UBatch the
	// physical batch size (--ubatch-size). A larger UBatch speeds up the
	// prompt and needs a larger compute buffer. 0 leaves llama-server's
	// default.
	Batch  int `yaml:"batch,omitempty" json:"batch,omitempty"`
	UBatch int `yaml:"ubatch,omitempty" json:"ubatch,omitempty"`
	// SplitMode is how a spanning model splits across the GPUs: layer (the
	// default) or row. It has no effect on one GPU.
	SplitMode string `yaml:"split_mode,omitempty" json:"split_mode,omitempty"`
	// Args are extra llama-server flags for anything the typed fields do not cover.
	Args []string `yaml:"args,omitempty" json:"args,omitempty"`
}

// MaxParallel is the most parallel slots that a model can have.
const MaxParallel = 64

// Limits of the batch sizes.
const (
	MinBatch = 32
	MaxBatch = 65536
)

// SplitModes are the split modes that a spanning model can use.
var SplitModes = []string{"layer", "row"}

var (
	safeArg     = regexp.MustCompile(`^[A-Za-z0-9_.,:=/+-]+$`)
	reservedArg = map[string]bool{
		"--model": true, "-m": true, "--host": true, "--port": true,
		"--device": true, "-dev": true, "--split-mode": true, "-sm": true,
		"--hf-repo": true, "-hf": true, "--api-key": true, "--api-key-file": true,
		"--chat-template-kwargs": true,
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
	if s.Reasoning != nil {
		if err := s.Reasoning.Validate(); err != nil {
			return err
		}
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
	for _, b := range []struct {
		name string
		v    int
	}{{"batch", s.Batch}, {"ubatch", s.UBatch}} {
		if b.v != 0 && (b.v < MinBatch || b.v > MaxBatch) {
			return fmt.Errorf("%s: must be %d to %d", b.name, MinBatch, MaxBatch)
		}
	}
	if s.Batch > 0 && s.UBatch > s.Batch {
		return fmt.Errorf("ubatch %d: must not be larger than batch %d", s.UBatch, s.Batch)
	}
	if s.SplitMode != "" && !slices.Contains(SplitModes, s.SplitMode) {
		return fmt.Errorf("split_mode %q: use one of %s", s.SplitMode, strings.Join(SplitModes, ", "))
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
	for id, s := range out {
		out[id] = s.moveBatchArgs()
	}
	return out, nil
}

// batchArgs are the raw args that became the typed Batch and UBatch.
var batchArgs = map[string]func(*Settings) *int{
	"-b": func(s *Settings) *int { return &s.Batch }, "--batch-size": func(s *Settings) *int { return &s.Batch },
	"-ub": func(s *Settings) *int { return &s.UBatch }, "--ubatch-size": func(s *Settings) *int { return &s.UBatch },
}

// moveBatchArgs moves batch flags from Args to the typed fields. Before the
// typed fields, the batch sizes could only be set as raw args.
func (s Settings) moveBatchArgs() Settings {
	var args []string
	for i := 0; i < len(s.Args); i++ {
		flag, val, eq := strings.Cut(s.Args[i], "=")
		field, ok := batchArgs[flag]
		if !ok {
			args = append(args, s.Args[i])
			continue
		}
		if !eq && i+1 < len(s.Args) {
			i++
			val = s.Args[i]
		}
		if n, err := strconv.Atoi(val); err == nil && *field(&s) == 0 {
			*field(&s) = n
		}
	}
	s.Args = args
	return s
}

// SaveSettings writes models.yaml atomically.
func SaveSettings(path string, s map[string]Settings) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return hardware.WriteFileAtomic(path, data, 0o640)
}
