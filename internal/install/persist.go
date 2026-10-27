package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PersistLabel is the XFS label of the persistence partition. XFS allows 12
// characters.
const PersistLabel = "SIBYLPERSIST"

// MinPersistSize is the smallest persistence partition that is worth making:
// the service images alone need about this much.
const MinPersistSize = 24 << 30

type sfdiskTable struct {
	PartitionTable struct {
		Label      string `json:"label"`
		Device     string `json:"device"`
		SectorSize int64  `json:"sectorsize"`
		Partitions []struct {
			Node  string `json:"node"`
			Start int64  `json:"start"`
			Size  int64  `json:"size"`
		} `json:"partitions"`
	} `json:"partitiontable"`
}

// Persist adds a persistence partition in the free space after the last
// partition of the live USB, and formats it. disk is the USB, such as
// /dev/sda. size is the size of the disk in bytes. It returns the new
// partition.
func Persist(ctx context.Context, run Runner, disk string, size int64) (string, error) {
	if out, _ := run(ctx, Cmd{Name: "blkid", Args: []string{"-L", PersistLabel}}); strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("a persistence partition already exists: %s", strings.TrimSpace(out))
	}
	out, err := run(ctx, Cmd{Name: "sfdisk", Args: []string{"--json", disk}})
	if err != nil {
		return "", err
	}
	var t sfdiskTable
	if err := json.Unmarshal([]byte(out), &t); err != nil {
		return "", fmt.Errorf("parse sfdisk --json: %w", err)
	}
	pt := t.PartitionTable
	if len(pt.Partitions) == 0 || pt.SectorSize == 0 {
		return "", fmt.Errorf("%s has no partitions: it does not look like the Sibyl live USB", disk)
	}
	var end int64
	for _, p := range pt.Partitions {
		end = max(end, p.Start+p.Size)
	}
	if free := size - end*pt.SectorSize; free < MinPersistSize {
		return "", fmt.Errorf("%s has %.1f GiB free after the live system: use a USB of at least 32 GB",
			disk, float64(free)/(1<<30))
	}

	if pt.Label == "gpt" {
		// The ISO puts the backup GPT header at the end of the ISO image, not
		// at the end of the USB. Move it, so the free space becomes usable.
		if _, err := run(ctx, Cmd{Name: "sfdisk", Args: []string{"--relocate", "gpt-bak-std", "--no-reread", disk}}); err != nil {
			return "", err
		}
	}
	// The live system runs from this USB, so the kernel cannot re-read the
	// whole table. partx adds only the new partition.
	if _, err := run(ctx, Cmd{Name: "sfdisk", Args: []string{"--append", "--no-reread", "--no-tell-kernel", disk},
		Stdin: ",,L\n"}); err != nil {
		return "", err
	}
	n := len(pt.Partitions) + 1
	part := Disk{Path: disk}.Partition(n)
	if _, err := run(ctx, Cmd{Name: "partx", Args: []string{"--add", "--nr", fmt.Sprint(n), disk}}); err != nil {
		return "", err
	}
	if _, err := run(ctx, Cmd{Name: "udevadm", Args: []string{"settle"}}); err != nil {
		return "", err
	}
	if _, err := run(ctx, Cmd{Name: "mkfs.xfs", Args: []string{"-f", "-L", PersistLabel, part}}); err != nil {
		return "", err
	}
	return part, nil
}

// ErrNotLive means the system did not boot from the live USB.
var ErrNotLive = errors.New("this is not the Sibyl live USB")
