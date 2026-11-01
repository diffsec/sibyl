package gpu

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Sample reads the live state of an amdgpu card from sysfs: load, memory,
// temperatures, power, fan and clocks. Units are in the key names. Other
// drivers do not report these files, so the result is empty for them.
func Sample(sysRoot string, g GPU) map[string]float64 {
	out := map[string]float64{}
	if g.card == "" {
		return out
	}
	dev := filepath.Join(sysRoot, "class", "drm", g.card, "device")
	for key, file := range map[string]string{
		"busy_percent":     "gpu_busy_percent",
		"mem_busy_percent": "mem_busy_percent",
		"vram_used_bytes":  "mem_info_vram_used",
		"vram_total_bytes": "mem_info_vram_total",
		"gtt_used_bytes":   "mem_info_gtt_used",
	} {
		if v, ok := readFloat(dev, file); ok {
			out[key] = v
		}
	}
	hwmons, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	if len(hwmons) == 0 {
		return out
	}
	hw := hwmons[0]
	// amdgpu labels temp1 edge, temp2 junction and temp3 mem.
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		label := readTrim(hw, "temp"+n+"_label")
		if label == "" {
			label = "temp" + n
		}
		if v, ok := readFloat(hw, "temp"+n+"_input"); ok {
			out["temp_"+label+"_c"] = v / 1000
		}
	}
	for _, f := range []string{"power1_average", "power1_input"} {
		if v, ok := readFloat(hw, f); ok {
			out["power_w"] = v / 1e6
			break
		}
	}
	if v, ok := readFloat(hw, "power1_cap"); ok {
		out["power_cap_w"] = v / 1e6
	}
	if v, ok := readFloat(hw, "fan1_input"); ok {
		out["fan_rpm"] = v
	}
	if v, ok := readFloat(hw, "freq1_input"); ok {
		out["sclk_mhz"] = v / 1e6
	}
	if v, ok := readFloat(hw, "freq2_input"); ok {
		out["mclk_mhz"] = v / 1e6
	}
	return out
}

// RecoverFile returns amdgpu's debugfs file that resets this GPU when read,
// or "" when there is none. Each debugfs/dri/<minor>/name names the PCI
// device that the minor belongs to.
func RecoverFile(sysRoot string, g GPU) string {
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "kernel", "debug", "dri", "*"))
	for _, d := range dirs {
		f := filepath.Join(d, "amdgpu_gpu_recover")
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if slices.Contains(strings.Fields(readTrim(d, "name")), "dev="+g.PCIAddr) {
			return f
		}
	}
	return ""
}

func readFloat(dir, name string) (float64, bool) {
	v, err := strconv.ParseFloat(readTrim(dir, name), 64)
	return v, err == nil
}
