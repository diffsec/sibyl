package hostd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ihavespoons/sibyl/internal/disks"
)

// MinPoolDisk is the smallest disk that can become a model pool.
const MinPoolDisk = 16 << 30

// PoolContext is the SELinux context of every file on a pool. The mount
// option sets it, so the containers can read the pool without a relabel.
const PoolContext = "system_u:object_r:container_file_t:s0"

// CreatePoolReq is the body of a pool-create request.
type CreatePoolReq struct {
	Disk string `json:"disk"`
	// Confirm is the disk's serial number, or "erase" when it has none.
	Confirm string `json:"confirm"`
}

// PoolName is the body of a pool-detach request.
type PoolName struct {
	Name string `json:"name"`
}

var (
	diskRE     = regexp.MustCompile(`^/dev/(sd[a-z]{1,2}|vd[a-z]{1,2}|nvme[0-9]{1,2}n[0-9]{1,2})$`)
	poolNameRE = regexp.MustCompile(`^pool-([0-9]{1,2})$`)
)

// Validate checks a pool-create request.
func (p CreatePoolReq) Validate() error {
	if !diskRE.MatchString(p.Disk) {
		return fmt.Errorf("disk %q: use a whole disk such as /dev/sdb or /dev/nvme1n1", p.Disk)
	}
	if strings.TrimSpace(p.Confirm) == "" {
		return errors.New("confirm: give the disk's serial number, or erase if it has none")
	}
	return nil
}

// Validate checks a pool name.
func (p PoolName) Validate() error {
	if !poolNameRE.MatchString(p.Name) {
		return fmt.Errorf("pool %q: use a name such as pool-1", p.Name)
	}
	return nil
}

// Pool is one model pool that sibyl-hostd created.
type Pool struct {
	Name     string `json:"name"`
	Disk     string `json:"disk"`
	Serial   string `json:"serial,omitempty"`
	LUKSUUID string `json:"luks_uuid"`
	Mount    string `json:"mount"`
	Mounted  bool   `json:"mounted"`
	Size     int64  `json:"size_bytes,omitempty"`
	Free     int64  `json:"free_bytes,omitempty"`
}

const poolsFile = "pools.json"

func (a *Agent) pools() []Pool {
	var p []Pool
	a.readData(poolsFile, &p)
	return p
}

func (a *Agent) mapperName(pool string) string { return "sibyl-" + pool }

// createPool erases a disk and makes it a model pool: one partition, LUKS2
// with a key file on the encrypted root disk, and XFS mounted under
// PoolsDir. A recovery key is the second way in, for a move to another host.
func (a *Agent) createPool(ctx context.Context, req CreatePoolReq) (string, string, error) {
	out, err := a.Run(ctx, "", "lsblk", disks.LsblkArgs...)
	if err != nil {
		return "", "", err
	}
	list, err := disks.Parse([]byte(out), nil, MinPoolDisk)
	if err != nil {
		return "", "", err
	}
	i := slices.IndexFunc(list, func(d disks.Disk) bool { return d.Path == req.Disk })
	if i < 0 {
		return "", "", fmt.Errorf("disk %s not found", req.Disk)
	}
	d := list[i]
	if d.Reason != "" {
		return "", "", fmt.Errorf("%s cannot be a pool: %s", d.Path, d.Reason)
	}
	want := d.Serial
	if want == "" {
		want = "erase"
	}
	if strings.TrimSpace(req.Confirm) != want {
		return "", "", fmt.Errorf("confirm does not match: give the serial number of %s (%s)", d.Path, want)
	}

	pools := a.pools()
	name := ""
	for n := 1; n < 100; n++ {
		c := "pool-" + strconv.Itoa(n)
		if !slices.ContainsFunc(pools, func(p Pool) bool { return p.Name == c }) {
			name = c
			break
		}
	}
	if name == "" {
		return "", "", errors.New("no free pool name")
	}
	mapper := a.mapperName(name)
	part := d.Partition(1)

	if _, err := a.Run(ctx, "", "wipefs", "--all", "--force", d.Path); err != nil {
		return "", "", err
	}
	table := "label: gpt\ntype=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"sibyl-" + name + "\"\n"
	if _, err := a.Run(ctx, table, "sfdisk", "--wipe", "always", "--wipe-partitions", "always", d.Path); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "udevadm", "settle"); err != nil {
		return "", "", err
	}

	keyDir := filepath.Join(a.Etc, "sibyl", "pools")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return "", "", err
	}
	key := make([]byte, 64)
	if _, err := rand.Read(key); err != nil {
		return "", "", err
	}
	keyFile := filepath.Join(keyDir, name+".key")
	if err := writeAtomic(keyFile, key, 0o400, "", -1); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "cryptsetup", "luksFormat", "--type", "luks2", "--batch-mode",
		"--label", mapper, "--key-file", keyFile, part); err != nil {
		return "", "", err
	}
	out, err = a.Run(ctx, "", "cryptsetup", "luksUUID", part)
	if err != nil {
		return "", "", err
	}
	uuid := strings.TrimSpace(out)
	out, err = a.Run(ctx, "", "systemd-cryptenroll", "--unlock-key-file="+keyFile, "--recovery-key", part)
	if err != nil {
		return "", "", err
	}
	recovery := recoveryKeyRE.FindString(out)
	if recovery == "" {
		return "", "", errors.New("systemd-cryptenroll printed no recovery key")
	}
	if _, err := a.Run(ctx, "", "cryptsetup", "open", "--key-file", keyFile, part, mapper); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "mkfs.xfs", "-f", "-L", name, "/dev/mapper/"+mapper); err != nil {
		return "", "", err
	}

	mount := filepath.Join(a.PoolsDir, name)
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return "", "", err
	}
	if err := a.editTable("crypttab", name, fmt.Sprintf("%s UUID=%s %s luks,nofail", mapper, uuid, keyFile)); err != nil {
		return "", "", err
	}
	if err := a.editTable("fstab", name, fmt.Sprintf("/dev/mapper/%s %s xfs nofail,context=\"%s\" 0 0", mapper, mount, PoolContext)); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "systemctl", "daemon-reload"); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "mount", mount); err != nil {
		return "", "", err
	}
	// The System API runs as sibyl and downloads into the pool.
	if _, err := a.Run(ctx, "", "chown", "sibyl:sibyl", mount); err != nil {
		return "", "", err
	}
	if _, err := a.Run(ctx, "", "chmod", "0750", mount); err != nil {
		return "", "", err
	}
	pools = append(pools, Pool{Name: name, Disk: d.Path, Serial: d.Serial, LUKSUUID: uuid, Mount: mount})
	if err := a.writeData(poolsFile, pools); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("created %s on %s", name, d.Path), recovery, nil
}

// detachPool unmounts a pool and removes it from crypttab and fstab. The
// data and the key file stay, so the pool can be attached again by hand.
func (a *Agent) detachPool(ctx context.Context, name string) (string, error) {
	pools := a.pools()
	i := slices.IndexFunc(pools, func(p Pool) bool { return p.Name == name })
	if i < 0 {
		return "", fmt.Errorf("no pool %s", name)
	}
	p := pools[i]
	if out, _ := a.Run(ctx, "", "findmnt", "-n", "-o", "TARGET", p.Mount); strings.TrimSpace(out) != "" {
		if _, err := a.Run(ctx, "", "umount", p.Mount); err != nil {
			return "", fmt.Errorf("%w: stop the models on this pool first", err)
		}
	}
	if _, err := os.Stat("/dev/mapper/" + a.mapperName(name)); err == nil {
		if _, err := a.Run(ctx, "", "cryptsetup", "close", a.mapperName(name)); err != nil {
			return "", err
		}
	}
	for _, f := range []string{"crypttab", "fstab"} {
		if err := a.editTable(f, name, ""); err != nil {
			return "", err
		}
	}
	if _, err := a.Run(ctx, "", "systemctl", "daemon-reload"); err != nil {
		return "", err
	}
	_ = os.Remove(p.Mount)
	if err := a.writeData(poolsFile, slices.Delete(pools, i, i+1)); err != nil {
		return "", err
	}
	return fmt.Sprintf("detached %s. Its data and key file %s stay", name,
		filepath.Join(a.Etc, "sibyl", "pools", name+".key")), nil
}

// editTable replaces the line for one pool in /etc/crypttab or /etc/fstab.
// Each line ends with a "# sibyl <pool>" marker. An empty line removes it.
func (a *Agent) editTable(file, pool, line string) error {
	path := filepath.Join(a.Etc, file)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	marker := " # sibyl " + pool
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if len(data) > 0 && !strings.HasSuffix(l, marker) {
			out = append(out, l)
		}
	}
	if line != "" {
		out = append(out, line+marker)
	}
	mode := os.FileMode(0o644)
	if file == "crypttab" {
		mode = 0o600
	}
	return writeAtomic(path, []byte(strings.Join(out, "\n")+"\n"), mode, "", -1)
}

// poolState returns the pools with their mount state and free space.
func (a *Agent) poolState(ctx context.Context) []Pool {
	pools := a.pools()
	for i := range pools {
		out, _ := a.Run(ctx, "", "findmnt", "-n", "-b", "-o", "SIZE,AVAIL", pools[i].Mount)
		if f := strings.Fields(out); len(f) == 2 {
			pools[i].Mounted = true
			pools[i].Size, _ = strconv.ParseInt(f[0], 10, 64)
			pools[i].Free, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	return pools
}

// recoveryKeyRE matches a systemd recovery key: 8 groups of 8 modhex characters.
var recoveryKeyRE = regexp.MustCompile(`[cbdefghijklnrtuv]{8}(-[cbdefghijklnrtuv]{8}){7}`)
