package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/sibyl/internal/disks"
)

const (
	testHash = "$6$salt$hash"
	testKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ben@laptop"
	recovery = "vbcrnjtk-ldrkfcug-khfivhtu-nhckhkhb-ttkfuvcb-hhgbnvcr-dlbkerkl-dteeikvc"
)

type fake struct {
	work  string
	calls []Cmd
	fail  string
}

func (f *fake) run(_ context.Context, c Cmd) (string, error) {
	f.calls = append(f.calls, c)
	if f.fail != "" && strings.HasPrefix(c.String(), f.fail) {
		return "", errors.New("failed")
	}
	switch {
	case c.String() == "cryptsetup luksUUID /dev/nvme0n1p3":
		return "1111-2222\n", nil
	case strings.Contains(c.String(), "--recovery-key"):
		return "A secret recovery key has been generated for this volume:\n\n    " + recovery + "\n", nil
	case c.Name == "podman":
		return "", os.MkdirAll(filepath.Join(f.work, "target/ostree/deploy/default/var"), 0o755)
	}
	return "", nil
}

// index returns the first call that equals prefix, or else the first that
// starts with it.
func (f *fake) index(prefix string) int {
	for i, c := range f.calls {
		if c.String() == prefix {
			return i
		}
	}
	for i, c := range f.calls {
		if strings.HasPrefix(c.String(), prefix) {
			return i
		}
	}
	return -1
}

func options() Options {
	return Options{
		Disk:    disks.Disk{Path: "/dev/nvme0n1", Size: 1 << 40},
		Payload: "ghcr.io/ihavespoons/sibyl-os:sha-1-amd",
		Target:  "ghcr.io/ihavespoons/sibyl-os:latest-amd",
		TPM:     true,
		Admin:   Admin{Name: "admin", Keys: []string{testKey}, PasswordHash: testHash},
	}
}

func TestInstall(t *testing.T) {
	work := t.TempDir()
	f := &fake{work: work}
	in := &Installer{Run: f.run, Work: work}
	res, err := in.Install(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveryKey != recovery || res.LUKSUUID != "1111-2222" {
		t.Fatalf("result %+v", res)
	}
	keyFile := filepath.Join(work, "luks.key")
	order := []string{
		"wipefs --all --force /dev/nvme0n1",
		"sfdisk --wipe always --wipe-partitions always /dev/nvme0n1",
		"cryptsetup luksFormat --type luks2 --batch-mode --label sibyl-root --key-file " + keyFile + " /dev/nvme0n1p3",
		"systemd-cryptenroll --unlock-key-file=" + keyFile + " --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3",
		"systemd-cryptenroll --unlock-key-file=" + keyFile + " --recovery-key /dev/nvme0n1p3",
		"cryptsetup open --key-file " + keyFile + " /dev/nvme0n1p3 luks-1111-2222",
		"cryptsetup luksRemoveKey --key-file " + keyFile + " /dev/nvme0n1p3",
		"mkfs.xfs -f -L root /dev/mapper/luks-1111-2222",
		"mount /dev/mapper/luks-1111-2222 " + work + "/target",
		"mount /dev/nvme0n1p2 " + work + "/target/boot",
		"mount /dev/nvme0n1p1 " + work + "/target/boot/efi",
		"podman run",
		"umount " + work + "/target/boot/efi",
		"umount " + work + "/target/boot",
		"umount " + work + "/target",
		"cryptsetup close luks-1111-2222",
	}
	last := -1
	for _, p := range order {
		i := f.index(p)
		if i <= last {
			t.Fatalf("%q at %d, after %d; calls:\n%v", p, i, last, f.calls)
		}
		last = i
	}
	pod := f.calls[f.index("podman run")].String()
	for _, want := range []string{"--source-imgref containers-storage:ghcr.io/ihavespoons/sibyl-os:sha-1-amd",
		"--target-imgref ghcr.io/ihavespoons/sibyl-os:latest-amd", "--karg rd.luks.uuid=luks-1111-2222", "/target"} {
		if !strings.Contains(pod, want) {
			t.Errorf("podman command lacks %q: %s", want, pod)
		}
	}
	if !strings.Contains(f.calls[f.index("sfdisk")].Stdin, `type=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709, name="root"`) {
		t.Error("sfdisk script has no root partition")
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("the key file is still on disk")
	}
	b, err := os.ReadFile(filepath.Join(work, "target/ostree/deploy/default/var", ProvisionFile))
	if err != nil {
		t.Fatal(err)
	}
	var a Admin
	if err := json.Unmarshal(b, &a); err != nil || a.Name != "admin" || a.PasswordHash != testHash {
		t.Fatalf("provision record %s, %v", b, err)
	}
}

func TestInstallPassphraseWithoutTPM(t *testing.T) {
	work := t.TempDir()
	f := &fake{work: work}
	o := options()
	o.TPM = false
	if _, err := (&Installer{Run: f.run, Work: work}).Install(context.Background(), o); err == nil || len(f.calls) > 0 {
		t.Fatalf("installed without a TPM and without a passphrase: %v", err)
	}
	o.Passphrase = "correct horse battery"
	if _, err := (&Installer{Run: f.run, Work: work}).Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if f.index("systemd-cryptenroll --unlock-key-file="+filepath.Join(work, "luks.key")+" --tpm2") >= 0 {
		t.Error("enrolled a TPM2 that is not there")
	}
	i := f.index("systemd-cryptenroll --unlock-key-file=" + filepath.Join(work, "luks.key") + " --password")
	if i < 0 || f.calls[i].Env[0] != "NEWPASSWORD=correct horse battery" {
		t.Fatalf("passphrase not enrolled: %v", f.calls)
	}
}

func TestInstallCleansUpAfterFailure(t *testing.T) {
	work := t.TempDir()
	f := &fake{work: work, fail: "podman run"}
	if _, err := (&Installer{Run: f.run, Work: work}).Install(context.Background(), options()); err == nil {
		t.Fatal("no error")
	}
	if f.index("umount "+work+"/target") < 0 || f.index("cryptsetup close luks-1111-2222") < 0 {
		t.Fatalf("no cleanup: %v", f.calls)
	}
}

func TestInstallRefusesBadTargets(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"boot disk":   func(o *Options) { o.Disk.Reason = "the running system boots from it" },
		"no password": func(o *Options) { o.Admin.PasswordHash = "" },
		"root":        func(o *Options) { o.Admin.Name = "root" },
		"bad key":     func(o *Options) { o.Admin.Keys = []string{"from=\"x\" " + testKey} },
		"short pass":  func(o *Options) { o.Passphrase = "short" },
	} {
		o := options()
		mod(&o)
		f := &fake{}
		if _, err := (&Installer{Run: f.run, Work: t.TempDir()}).Install(context.Background(), o); err == nil || len(f.calls) > 0 {
			t.Errorf("%s: err %v, %d commands", name, err, len(f.calls))
		}
	}
}

func TestProvision(t *testing.T) {
	stateVar := t.TempDir()
	if err := writeProvision(stateVar, options().Admin); err != nil {
		t.Fatal(err)
	}
	var calls []Cmd
	run := func(_ context.Context, c Cmd) (string, error) { calls = append(calls, c); return "", nil }
	msg, err := Provision(context.Background(), run, stateVar)
	if err != nil || msg == "" {
		t.Fatalf("%q %v", msg, err)
	}
	if calls[1].String() != "useradd --create-home --user-group --groups wheel,sibyl-operators --password "+testHash+" --comment Sibyl administrator admin" {
		t.Fatalf("useradd: %s", calls[1])
	}
	if calls[2].Stdin != testKey+"\n" {
		t.Fatalf("keys %q", calls[2].Stdin)
	}
	if _, err := os.Stat(filepath.Join(stateVar, ProvisionFile)); !os.IsNotExist(err) {
		t.Fatal("record not removed")
	}
	if msg, err := Provision(context.Background(), run, stateVar); msg != "" || err != nil {
		t.Fatalf("second run: %q %v", msg, err)
	}
}
