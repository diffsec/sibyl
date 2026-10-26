// Package hardware loads and saves the setup wizard's hardware choices.
package hardware

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Backend is the llama.cpp GPU backend.
type Backend string

const (
	BackendHIP    Backend = "hip"
	BackendVulkan Backend = "vulkan"
)

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

// Default is used before the setup wizard runs: HIP on GPU 0 only.
func Default() Config {
	return Config{Backend: BackendHIP, GPUs: []int{0}, Layout: LayoutSpan}
}

// Validate checks the configuration against the number of detected GPUs.
func (c Config) Validate(detected int) error {
	switch c.Backend {
	case BackendHIP, BackendVulkan:
	default:
		return fmt.Errorf("backend %q: must be %q or %q", c.Backend, BackendHIP, BackendVulkan)
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

// Load reads path. A missing file returns Default.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
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
