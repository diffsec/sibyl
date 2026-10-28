package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCard builds a sysfs-like tree for one card: class/drm/<card> -> devices/<addr>.
func fakeCard(t *testing.T, root, card, addr string, files map[string]string) {
	t.Helper()
	devDir := filepath.Join(root, "devices", addr)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(devDir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cardDir := filepath.Join(root, "class", "drm", card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devDir, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestDetectOrdersByPCIAndSkipsConnectors(t *testing.T) {
	root := t.TempDir()
	xtx := map[string]string{
		"vendor":              "0x1002",
		"device":              "0x744c",
		"mem_info_vram_total": "25753026560",
		"mem_info_vram_used":  "104857600",
		"current_link_width":  "16",
		"current_link_speed":  "16.0 GT/s PCIe",
	}
	// card0 has the higher PCI address, so it must sort second.
	fakeCard(t, root, "card0", "0000:83:00.0", xtx)
	fakeCard(t, root, "card1", "0000:03:00.0", xtx)
	if err := os.MkdirAll(filepath.Join(root, "class", "drm", "card0-DP-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "class", "drm", "renderD128"), 0o755); err != nil {
		t.Fatal(err)
	}

	gpus, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(gpus))
	}
	if gpus[0].PCIAddr != "0000:03:00.0" || gpus[0].Index != 0 {
		t.Errorf("first GPU = %s index %d, want 0000:03:00.0 index 0", gpus[0].PCIAddr, gpus[0].Index)
	}
	if gpus[1].Index != 1 {
		t.Errorf("second GPU index = %d, want 1", gpus[1].Index)
	}
	g := gpus[0]
	if g.Vendor != "amd" || g.DeviceID != "0x744c" || g.VRAMTotal != 25753026560 || g.LinkWidth != "16" {
		t.Errorf("unexpected fields: %+v", g)
	}
}

func TestDetectMissingDRM(t *testing.T) {
	if _, err := Detect(t.TempDir()); err == nil {
		t.Fatal("want an error when class/drm does not exist")
	}
}

func TestArch(t *testing.T) {
	for v, want := range map[uint64]string{0: "", 110000: "gfx1100", 100300: "gfx1030", 110501: "gfx1151", 120001: "gfx1201", 90010: "gfx90a", 90402: "gfx942"} {
		if got := Arch(v); got != want {
			t.Errorf("Arch(%d) = %q, want %q", v, got, want)
		}
	}
}

// TestDetectArch maps amdkfd topology nodes to cards by their render node,
// as on the test host: node 1 is gfx1100 at 0000:07:00.0.
func TestDetectArch(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card1", "0000:07:00.0", map[string]string{"vendor": VendorAMD})
	fakeCard(t, root, "card2", "0000:0b:00.0", map[string]string{"vendor": VendorAMD})
	for minor, addr := range map[string]string{"128": "0000:07:00.0", "129": "0000:0b:00.0"} {
		dir := filepath.Join(root, "class", "drm", "renderD"+minor)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "devices", addr), filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
	}
	nodes := map[string]string{
		"0": "gfx_target_version 0\ndrm_render_minor 0\n",
		"1": "gfx_target_version 110000\nlocation_id 1792\ndrm_render_minor 128\n",
		"2": "gfx_target_version 100302\nlocation_id 2816\ndrm_render_minor 129\n",
	}
	for n, props := range nodes {
		dir := filepath.Join(root, "class", "kfd", "kfd", "topology", "nodes", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "properties"), []byte(props), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gpus, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0].Arch != "gfx1100" || gpus[1].Arch != "gfx1032" {
		t.Errorf("gpus = %+v", gpus)
	}
}
