package diag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/gpu"
)

// Inventory lists the hardware and software of the box. It leaves out
// serial numbers, MAC addresses and the machine ID, because it goes into
// every bundle.
func (d *Diag) Inventory(ctx context.Context) map[string]any {
	run := func(name string, args ...string) string {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := d.Run(cctx, name, args...)
		if err != nil && out == "" {
			return ""
		}
		return strings.TrimSpace(out)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(d.path(rel))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	gpus, _ := gpu.Detect(d.path("sys"))
	board := map[string]string{}
	for _, f := range []string{"sys_vendor", "product_name", "board_vendor", "board_name", "bios_vendor",
		"bios_version", "bios_date"} {
		board[f] = read("sys/class/dmi/id/" + f)
	}
	vbios := map[string]string{}
	for _, g := range gpus {
		vbios[g.PCIAddr] = read("sys/bus/pci/devices/" + g.PCIAddr + "/vbios_version")
	}
	var watchdogs []map[string]string
	wds, _ := filepath.Glob(d.path("sys/class/watchdog/watchdog*"))
	for _, wd := range wds {
		rel := strings.TrimPrefix(wd, d.path()+"/")
		watchdogs = append(watchdogs, map[string]string{
			"name": filepath.Base(wd), "identity": read(rel + "/identity"),
			"timeout": read(rel + "/timeout"), "bootstatus": read(rel + "/bootstatus"),
		})
	}
	var disks any = run("lsblk", "-J", "-b", "-o", "NAME,TYPE,SIZE,MODEL,ROTA,TRAN,FSTYPE,MOUNTPOINTS")
	var parsed any
	if json.Unmarshal([]byte(disks.(string)), &parsed) == nil {
		disks = parsed
	}
	mem := meminfo(d.path("proc/meminfo"))
	return map[string]any{
		"sibyl": map[string]string{
			"version": d.readShare("version"), "image_tag": d.readShare("image-tag"),
			"gpu_variant": d.readShare("gpu-variant"),
		},
		"os_release":       osRelease(read("etc/os-release")),
		"kernel":           run("uname", "-r"),
		"kernel_cmdline":   read("proc/cmdline"),
		"bootc":            d.bootcSummary(run("bootc", "status", "--json")),
		"cpu":              cpuInfo(read("proc/cpuinfo")),
		"memory_bytes":     mem["MemTotal"],
		"swap_bytes":       mem["SwapTotal"],
		"board":            board,
		"gpus":             gpus,
		"vbios":            vbios,
		"disks":            disks,
		"watchdog_devices": watchdogs,
		"pstore_backend":   read("sys/module/pstore/parameters/backend"),
		"efi":              read("sys/firmware/efi/fw_platform_size") != "",
		"secure_boot":      run("mokutil", "--sb-state"),
		"arch":             runtime.GOARCH,
	}
}

func osRelease(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && slices.Contains([]string{"NAME", "VERSION_ID", "VARIANT_ID", "PRETTY_NAME"}, k) {
			out[k] = strings.Trim(v, `"`)
		}
	}
	return out
}

func cpuInfo(text string) map[string]any {
	model, flags, n := "", "", 0
	for _, line := range strings.Split(text, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "model name":
			n++
			if model == "" {
				model = strings.TrimSpace(v)
			}
		case "flags":
			if flags == "" {
				flags = v
			}
		}
	}
	var have []string
	fields := strings.Fields(flags)
	for _, f := range []string{"avx2", "avx512f", "avx512_bf16", "avx_vnni", "amx_tile"} {
		if slices.Contains(fields, f) {
			have = append(have, f)
		}
	}
	return map[string]any{"model": model, "logical_cpus": n, "flags": have}
}

// bootcSummary keeps the image reference and digest of each slot.
func (d *Diag) bootcSummary(raw string) map[string]any {
	type entry struct {
		Image *struct {
			Image struct {
				Image string `json:"image"`
			} `json:"image"`
			Digest  string `json:"imageDigest"`
			Version string `json:"version"`
		} `json:"image"`
	}
	// status also holds fields that are not slots, such as "type", so the
	// slots are named fields and not a map.
	var st struct {
		Status struct {
			Booted   *entry `json:"booted"`
			Staged   *entry `json:"staged"`
			Rollback *entry `json:"rollback"`
		} `json:"status"`
	}
	out := map[string]any{}
	if json.Unmarshal([]byte(raw), &st) != nil {
		return out
	}
	for slot, e := range map[string]*entry{"booted": st.Status.Booted, "staged": st.Status.Staged, "rollback": st.Status.Rollback} {
		if e != nil && e.Image != nil {
			out[slot] = map[string]string{"image": e.Image.Image.Image, "digest": e.Image.Digest, "version": e.Image.Version}
		}
	}
	return out
}
