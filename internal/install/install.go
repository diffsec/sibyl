package install

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/diffsec/sibyl/internal/disks"
	"github.com/diffsec/sibyl/internal/hostd"
)

// Cmd is one command to run.
type Cmd struct {
	Name  string
	Args  []string
	Stdin string
	Env   []string // added to a fixed base environment
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, c Cmd) (string, error)

// Admin is the first login account of the installed system. It is in wheel,
// so it can use sudo with its password.
type Admin struct {
	Name         string   `json:"name"`
	Keys         []string `json:"keys,omitempty"`
	PasswordHash string   `json:"password_hash"`
}

var adminRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Validate checks the admin account.
func (a Admin) Validate() error {
	if !adminRE.MatchString(a.Name) || slices.Contains([]string{"root", "sibyl", "nobody"}, a.Name) {
		return fmt.Errorf("user name %q: use up to 32 lowercase letters, digits, _ and -, and not root or sibyl", a.Name)
	}
	if !strings.HasPrefix(a.PasswordHash, "$6$") && !strings.HasPrefix(a.PasswordHash, "$y$") {
		return errors.New("the admin account needs a password: it is the console login and the sudo password")
	}
	for i, k := range a.Keys {
		if _, err := hostd.ParseKey(k); err != nil {
			return fmt.Errorf("keys[%d]: %w", i, err)
		}
	}
	return nil
}

// MinDiskSize is the smallest disk the installer offers. The image, its
// rollback copy and one small model need about this much.
const MinDiskSize = 64 << 30

// Options are the choices for one install.
type Options struct {
	Disk disks.Disk
	// Payload is the image in local container storage that is installed.
	Payload string
	// Target is the registry image that bootc upgrade follows afterwards.
	Target string
	// TPM binds the root partition to the TPM2 with PCR 7.
	TPM bool
	// Passphrase is an optional extra unlock passphrase. Without a TPM it is
	// required.
	Passphrase string
	Admin      Admin
}

// Validate checks the options before anything is written.
func (o Options) Validate() error {
	if o.Disk.Reason != "" {
		return fmt.Errorf("%s cannot be the target: %s", o.Disk.Path, o.Disk.Reason)
	}
	if !strings.HasPrefix(o.Disk.Path, "/dev/") {
		return fmt.Errorf("disk %q is not a device path", o.Disk.Path)
	}
	if o.Payload == "" || o.Target == "" {
		return errors.New("the image to install is not set")
	}
	if !o.TPM && o.Passphrase == "" {
		return errors.New("this machine has no TPM2, so the disk needs a passphrase")
	}
	if o.Passphrase != "" && len(o.Passphrase) < 12 {
		return errors.New("use a passphrase of at least 12 characters")
	}
	return o.Admin.Validate()
}

// Result is what the user must keep after an install.
type Result struct {
	RecoveryKey string
	LUKSUUID    string
}

// Installer runs the install steps.
type Installer struct {
	Run Runner
	// Work is a directory on tmpfs for the key file and the mount points.
	Work string
	// Step reports progress.
	Step func(string)
	// Debug makes bootc log at trace level.
	Debug bool
}

// Partition layout. The ESP and /boot match a Fedora bootc install, and root
// takes the rest of the disk.
const (
	espSize  = "600MiB"
	bootSize = "2GiB"
	// ProvisionFile is where the admin account waits for its first boot,
	// relative to the target's /var.
	ProvisionFile = "lib/sibyl-install/admin.json"
)

var sfdiskScript = `label: gpt
size=` + espSize + `, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name="EFI-SYSTEM"
size=` + bootSize + `, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="boot"
type=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709, name="root"
`

// Install wipes the disk and installs Sibyl. It closes and unmounts
// everything on return, also after a failure.
func (in *Installer) Install(ctx context.Context, o Options) (res Result, err error) {
	if err := o.Validate(); err != nil {
		return res, err
	}
	run := func(step string, c Cmd) (string, error) {
		if step != "" && in.Step != nil {
			in.Step(step)
		}
		out, err := in.Run(ctx, c)
		if err != nil {
			return out, fmt.Errorf("%s: %w", c.Name, err)
		}
		return out, nil
	}
	var cleanup []Cmd
	defer func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			_, _ = in.Run(context.WithoutCancel(ctx), cleanup[i])
		}
	}()

	esp, boot, root := o.Disk.Partition(1), o.Disk.Partition(2), o.Disk.Partition(3)
	if _, err := run("Wiping "+o.Disk.Path, Cmd{Name: "wipefs", Args: []string{"--all", "--force", o.Disk.Path}}); err != nil {
		return res, err
	}
	if _, err := run("Partitioning", Cmd{Name: "sfdisk", Args: []string{"--wipe", "always", "--wipe-partitions", "always", o.Disk.Path}, Stdin: sfdiskScript}); err != nil {
		return res, err
	}
	if _, err := run("", Cmd{Name: "udevadm", Args: []string{"settle"}}); err != nil {
		return res, err
	}
	if _, err := run("Formatting the boot partitions", Cmd{Name: "mkfs.fat", Args: []string{"-F", "32", "-n", "EFI-SYSTEM", esp}}); err != nil {
		return res, err
	}
	if _, err := run("", Cmd{Name: "mkfs.xfs", Args: []string{"-f", "-L", "boot", boot}}); err != nil {
		return res, err
	}

	// A random key file starts the LUKS header. The TPM2, the recovery key
	// and the optional passphrase are enrolled with it, and then it is removed.
	keyFile := filepath.Join(in.Work, "luks.key")
	key := make([]byte, 64)
	if _, err := rand.Read(key); err != nil {
		return res, err
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(keyFile) }()

	if _, err := run("Encrypting the root partition", Cmd{Name: "cryptsetup", Args: []string{"luksFormat", "--type", "luks2", "--batch-mode",
		"--label", "sibyl-root", "--key-file", keyFile, root}}); err != nil {
		return res, err
	}
	out, err := run("", Cmd{Name: "cryptsetup", Args: []string{"luksUUID", root}})
	if err != nil {
		return res, err
	}
	res.LUKSUUID = strings.TrimSpace(out)
	mapper := "luks-" + res.LUKSUUID

	if o.TPM {
		if _, err := run("Binding the disk to the TPM2 (PCR 7)", Cmd{Name: "systemd-cryptenroll",
			Args: []string{"--unlock-key-file=" + keyFile, "--tpm2-device=auto", "--tpm2-pcrs=7", root}}); err != nil {
			return res, err
		}
	}
	out, err = run("Creating the recovery key", Cmd{Name: "systemd-cryptenroll",
		Args: []string{"--unlock-key-file=" + keyFile, "--recovery-key", root}})
	if err != nil {
		return res, err
	}
	if res.RecoveryKey = recoveryKeyRE.FindString(out); res.RecoveryKey == "" {
		return res, errors.New("systemd-cryptenroll printed no recovery key")
	}
	if o.Passphrase != "" {
		if _, err := run("Adding the passphrase", Cmd{Name: "systemd-cryptenroll",
			Args: []string{"--unlock-key-file=" + keyFile, "--password", root}, Env: []string{"NEWPASSWORD=" + o.Passphrase}}); err != nil {
			return res, err
		}
	}
	if _, err := run("", Cmd{Name: "cryptsetup", Args: []string{"open", "--key-file", keyFile, root, mapper}}); err != nil {
		return res, err
	}
	cleanup = append(cleanup, Cmd{Name: "cryptsetup", Args: []string{"close", mapper}})
	if _, err := run("", Cmd{Name: "cryptsetup", Args: []string{"luksRemoveKey", "--key-file", keyFile, root}}); err != nil {
		return res, err
	}
	dev := "/dev/mapper/" + mapper
	if _, err := run("Formatting the root file system", Cmd{Name: "mkfs.xfs", Args: []string{"-f", "-L", "root", dev}}); err != nil {
		return res, err
	}

	target := filepath.Join(in.Work, "target")
	for _, m := range []struct{ dev, dir string }{{dev, target}, {boot, target + "/boot"}, {esp, target + "/boot/efi"}} {
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return res, err
		}
		if _, err := run("", Cmd{Name: "mount", Args: []string{m.dev, m.dir}}); err != nil {
			return res, err
		}
	}
	// bootc adds its own mounts under the target, so unmount the whole tree.
	cleanup = append(cleanup, Cmd{Name: "umount", Args: []string{"--recursive", target}})

	logLevel := "info"
	if in.Debug {
		logLevel = "trace"
	}
	// bootc runs on the live system itself, which is the same image. It
	// reads the embedded image from container storage. Running it inside a
	// container of that image fails on the live system: the image sits in a
	// read-only image store on the squashfs, and crun cannot find /tmp in the
	// nested overlay.
	// bootc makes its own mount namespace only inside a container. unshare
	// gives it a private one here, so its mounts, such as /run/bootc/storage,
	// go away when it exits.
	if _, err := run("Installing "+o.Target+" (this takes a few minutes)", Cmd{Name: "unshare", Args: []string{
		"--mount", "--propagation", "private", "--",
		"bootc", "install", "to-filesystem",
		"--source-imgref", "containers-storage:" + o.Payload,
		"--target-imgref", o.Target,
		"--skip-fetch-check",
		"--karg", "rd.luks.uuid=" + mapper,
		target}, Env: []string{"RUST_LOG=" + logLevel}}); err != nil {
		return res, err
	}

	if in.Step != nil {
		in.Step("Writing the admin account for the first boot")
	}
	// bootc's last step remounts the target read-only.
	if _, err := run("", Cmd{Name: "mount", Args: []string{"-o", "remount,rw", target}}); err != nil {
		return res, err
	}
	if err := writeProvision(filepath.Join(target, "ostree/deploy/default/var"), o.Admin); err != nil {
		return res, err
	}
	if _, err := run("", Cmd{Name: "sync"}); err != nil {
		return res, err
	}
	return res, nil
}

// recoveryKeyRE matches a systemd recovery key: 8 groups of 8 modhex characters.
var recoveryKeyRE = regexp.MustCompile(`[cbdefghijklnrtuv]{8}(-[cbdefghijklnrtuv]{8}){7}`)

func writeProvision(stateVar string, a Admin) error {
	if fi, err := os.Stat(stateVar); err != nil || !fi.IsDir() {
		return fmt.Errorf("bootc did not create %s", stateVar)
	}
	dir := filepath.Join(stateVar, filepath.Dir(ProvisionFile))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateVar, ProvisionFile), b, 0o600)
}
