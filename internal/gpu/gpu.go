// Package gpu detects GPUs from sysfs.
package gpu

import (
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
	for i := range gpus {
		gpus[i].Index = i
	}
	return gpus, nil
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
