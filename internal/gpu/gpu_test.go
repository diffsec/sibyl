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
