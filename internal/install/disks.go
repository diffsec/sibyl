// Package install is the guided disk installer. It runs as root on the live
// USB. It partitions one disk, encrypts the root partition with LUKS2 bound
// to the TPM2, and installs the Sibyl image with bootc install to-filesystem.
//
// bootc's own --block-setup tpm2-luks keeps no recovery key, so the
// installer sets up LUKS itself, as the bootc documentation recommends.
package install

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// MinDiskSize is the smallest disk the installer offers. The image, its
// rollback copy and one small model need about this much.
const MinDiskSize = 64 << 30

// Disk is one whole disk from lsblk.
type Disk struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Model     string `json:"model"`
	Serial    string `json:"serial"`
	Transport string `json:"tran"`
	Removable bool   `json:"rm"`
	ReadOnly  bool   `json:"ro"`
	// Mounted lists the mount points on the disk and its partitions.
	Mounted []string `json:"mounted,omitempty"`
	// Reason is why the disk cannot be the target, or empty.
	Reason string `json:"reason,omitempty"`
}

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Size        int64      `json:"size"`
	Model       *string    `json:"model"`
	Serial      *string    `json:"serial"`
	Type        string     `json:"type"`
	Tran        *string    `json:"tran"`
	RM          bool       `json:"rm"`
	RO          bool       `json:"ro"`
	Mountpoints []*string  `json:"mountpoints"`
	Children    []lsblkDev `json:"children"`
}

// LsblkArgs are the lsblk arguments that ParseDisks reads.
var LsblkArgs = []string{"-J", "-b", "-o", "NAME,PATH,SIZE,MODEL,SERIAL,TYPE,TRAN,RM,RO,MOUNTPOINTS"}

// ParseDisks reads lsblk JSON and returns every whole disk. bootDisks are
// the disks that hold the running system, such as the live USB. They, and
// disks that are too small, read-only or in use, get a Reason.
func ParseDisks(data []byte, bootDisks []string) ([]Disk, error) {
	var out struct {
		Devices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}
	var disks []Disk
	for _, d := range out.Devices {
		if d.Type != "disk" || strings.HasPrefix(d.Name, "zram") {
			continue
		}
		disk := Disk{Name: d.Name, Path: d.Path, Size: d.Size, Model: str(d.Model), Serial: str(d.Serial),
			Transport: str(d.Tran), Removable: d.RM, ReadOnly: d.RO, Mounted: mounts(d)}
		switch {
		case slices.Contains(bootDisks, d.Path):
			disk.Reason = "the running system boots from it"
		case d.RO:
			disk.Reason = "read-only"
		case d.Size < MinDiskSize:
			disk.Reason = fmt.Sprintf("smaller than %d GiB", MinDiskSize>>30)
		case len(disk.Mounted) > 0:
			disk.Reason = "in use: mounted at " + strings.Join(disk.Mounted, ", ")
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

func mounts(d lsblkDev) []string {
	var m []string
	for _, p := range d.Mountpoints {
		if p != nil && *p != "" && *p != "[SWAP]" {
			m = append(m, *p)
		}
	}
	for _, c := range d.Children {
		m = append(m, mounts(c)...)
	}
	return m
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// Label is a one-line description of a disk for a confirmation prompt.
func (d Disk) Label() string {
	parts := []string{d.Path, fmt.Sprintf("%.0f GB", float64(d.Size)/1e9)}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	if d.Serial != "" {
		parts = append(parts, "serial "+d.Serial)
	}
	if d.Transport != "" {
		parts = append(parts, d.Transport)
	}
	return strings.Join(parts, "  ")
}

// Partition returns the path of partition n on the disk: /dev/sda2 or
// /dev/nvme0n1p2.
func (d Disk) Partition(n int) string {
	last := d.Path[len(d.Path)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", d.Path, n)
	}
	return fmt.Sprintf("%s%d", d.Path, n)
}
