// Package gpu detects GPUs from sysfs.
package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Vendor PCI IDs.
const (
	VendorAMD    = "0x1002"
	VendorNVIDIA = "0x10de"
	VendorIntel  = "0x8086"
)

// GPU is one detected card.
type GPU struct {
	// Index is the position in the detected list, ordered by PCI address.
	Index     int    `json:"index"`
	PCIAddr   string `json:"pci_address"`
	Vendor    string `json:"vendor"`
	VendorID  string `json:"vendor_id"`
	DeviceID  string `json:"device_id"`
	Driver    string `json:"driver"`
	VRAMTotal uint64 `json:"vram_total_bytes,omitempty"`
	VRAMUsed  uint64 `json:"vram_used_bytes,omitempty"`
	LinkWidth string `json:"pcie_link_width,omitempty"`
	LinkSpeed string `json:"pcie_link_speed,omitempty"`
	// Arch is the ROCm target of an AMD GPU, such as gfx1100. It is empty
	// when the amdkfd driver does not report one.
	Arch string `json:"arch,omitempty"`
	// card is the DRM directory, such as card1, that Sample reads.
	card string
}

var cardName = regexp.MustCompile(`^card[0-9]+$`)

// Detect lists the GPUs under <sysRoot>/class/drm. sysRoot is normally "/sys".
//
// The order is by PCI address. ROCm and Vulkan usually enumerate devices in
// the same order, but the setup wizard confirms it with llama-server
// --list-devices before it saves a selection.
func Detect(sysRoot string) ([]GPU, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "drm"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var gpus []GPU
	for _, e := range entries {
		if !cardName.MatchString(e.Name()) {
			continue // connectors such as card0-DP-1, and render nodes
		}
		dev := filepath.Join(sysRoot, "class", "drm", e.Name(), "device")
		real, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		addr := filepath.Base(real)
		if seen[addr] {
			continue
		}
		seen[addr] = true

		g := GPU{
			card:      e.Name(),
			PCIAddr:   addr,
			VendorID:  readTrim(dev, "vendor"),
			DeviceID:  readTrim(dev, "device"),
			LinkWidth: readTrim(dev, "current_link_width"),
			LinkSpeed: readTrim(dev, "current_link_speed"),
			VRAMTotal: readUint(dev, "mem_info_vram_total"),
			VRAMUsed:  readUint(dev, "mem_info_vram_used"),
		}
		if drv, err := filepath.EvalSymlinks(filepath.Join(dev, "driver")); err == nil {
			g.Driver = filepath.Base(drv)
		}
		g.Vendor = vendorName(g.VendorID)
		gpus = append(gpus, g)
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].PCIAddr < gpus[j].PCIAddr })
	archs := kfdArchs(sysRoot)
	for i := range gpus {
		gpus[i].Index = i
		gpus[i].Arch = archs[gpus[i].PCIAddr]
	}
	return gpus, nil
}

// kfdArchs maps PCI addresses to ROCm targets. amdkfd lists one topology
// node per GPU, with its target version and its render node. A missing
// topology, as on NVIDIA and Intel hosts, gives an empty map.
func kfdArchs(sysRoot string) map[string]string {
	out := map[string]string{}
	nodes, err := filepath.Glob(filepath.Join(sysRoot, "class", "kfd", "kfd", "topology", "nodes", "*", "properties"))
	if err != nil {
		return out
	}
	for _, p := range nodes {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		props := map[string]uint64{}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					props[f[0]] = v
				}
			}
		}
		arch := Arch(props["gfx_target_version"])
		minor, ok := props["drm_render_minor"]
		if arch == "" || !ok {
			continue // a CPU node has version 0
		}
		dev := filepath.Join(sysRoot, "class", "drm", "renderD"+strconv.FormatUint(minor, 10), "device")
		if real, err := filepath.EvalSymlinks(dev); err == nil {
			out[filepath.Base(real)] = arch
		}
	}
	return out
}

// Arch turns amdkfd's gfx_target_version into a ROCm target name. The
// version is major*10000 + minor*100 + stepping, and the name writes minor
// and stepping as one hex digit each: 110000 is gfx1100, 90010 is gfx90a.
func Arch(version uint64) string {
	if version == 0 {
		return ""
	}
	major, minor, step := version/10000, version/100%100, version%100
	if minor > 15 || step > 15 {
		return ""
	}
	return fmt.Sprintf("gfx%d%x%x", major, minor, step)
}

func vendorName(id string) string {
	switch id {
	case VendorAMD:
		return "amd"
	case VendorNVIDIA:
		return "nvidia"
	case VendorIntel:
		return "intel"
	default:
		return "unknown"
	}
}

func readTrim(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(dir, name string) uint64 {
	v, err := strconv.ParseUint(readTrim(dir, name), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
