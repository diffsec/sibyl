// Package hardware loads and saves the setup wizard's hardware choices.
package hardware

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Backend is the llama.cpp GPU backend.
type Backend string

const (
	BackendHIP    Backend = "hip"
	BackendCUDA   Backend = "cuda"
	BackendVulkan Backend = "vulkan"
)

// Variant is the GPU variant of the host image: amd, nvidia or intel.
type Variant string

const (
	VariantAMD    Variant = "amd"
	VariantNVIDIA Variant = "nvidia"
	VariantIntel  Variant = "intel"
)

// Backends returns the backends that a variant ships, the default first.
// Vulkan runs on every vendor. An unknown variant is treated as amd.
func Backends(v Variant) []Backend {
	switch v {
	case VariantNVIDIA:
		return []Backend{BackendCUDA, BackendVulkan}
	case VariantIntel:
		return []Backend{BackendVulkan}
	default:
		return []Backend{BackendHIP, BackendVulkan}
	}
}

// Layout is the default placement of a model on the selected GPUs.
type Layout string

const (
	// LayoutSpan splits one model across every selected GPU.
	LayoutSpan Layout = "span"
	// LayoutPin places each model on one GPU, so several models run at once.
	LayoutPin Layout = "pin"
)

// Config is the content of hardware.yaml.
type Config struct {
	Backend Backend `yaml:"backend" json:"backend"`
	// GPUs lists the selected cards as indexes into the detected GPU list.
	GPUs   []int  `yaml:"gpus" json:"gpus"`
	Layout Layout `yaml:"layout" json:"layout"`
}

// Default is used before the setup wizard runs: the variant's first
// backend, on GPU 0 only.
func Default(v Variant) Config {
	return Config{Backend: Backends(v)[0], GPUs: []int{0}, Layout: LayoutSpan}
}

// Validate checks the configuration against the variant and the number of
// detected GPUs.
func (c Config) Validate(v Variant, detected int) error {
	if !slices.Contains(Backends(v), c.Backend) {
		return fmt.Errorf("backend %q: the %s variant ships %v", c.Backend, v, Backends(v))
	}
	switch c.Layout {
	case LayoutSpan, LayoutPin:
	default:
		return fmt.Errorf("layout %q: must be %q or %q", c.Layout, LayoutSpan, LayoutPin)
	}
	if len(c.GPUs) == 0 {
		return errors.New("gpus: select at least one GPU")
	}
	seen := map[int]bool{}
	for _, g := range c.GPUs {
		if g < 0 || g >= detected {
			return fmt.Errorf("gpus: index %d is out of range (detected %d)", g, detected)
		}
		if seen[g] {
			return fmt.Errorf("gpus: index %d is listed twice", g)
		}
		seen[g] = true
	}
	return nil
}

// Load reads path. A missing file returns the variant's Default.
func Load(path string, v Variant) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(v), nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes path atomically.
func Save(path string, c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o640)
}

// WriteFileAtomic writes data to a temporary file in the same directory and
// renames it over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
