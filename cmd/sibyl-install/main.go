// Command sibyl-install installs Sibyl from the live USB to a disk.
//
//	sibyl-install                 ask the questions, then install
//	sibyl-install -config f.json  install without questions (for tests)
//	sibyl-install disks           list the disks as JSON
//	sibyl-install persist         add a persistence partition to the live USB
//	sibyl-install provision       create the admin account at the first boot
//
// It must run as root.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"charm.land/huh/v2"

	"github.com/ihavespoons/sibyl/internal/disks"
	"github.com/ihavespoons/sibyl/internal/hostd"
	"github.com/ihavespoons/sibyl/internal/install"
)

const registry = "ghcr.io/ihavespoons/sibyl-os"

func main() {
	config := flag.String("config", "", "JSON file with the install choices: no questions are asked")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch flag.Arg(0) {
	case "provision":
		var msg string
		msg, err = install.Provision(ctx, run, "/var")
		if msg != "" {
			fmt.Println(msg)
		}
	case "disks":
		var list []disks.Disk
		if list, err = listDisks(ctx); err == nil {
			err = json.NewEncoder(os.Stdout).Encode(list)
		}
	case "persist":
		err = persistCmd(ctx)
	case "":
		err = installCmd(ctx, *config)
	default:
		err = fmt.Errorf("unknown command %q", flag.Arg(0))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sibyl-install:", err)
		os.Exit(1)
	}
}

// run runs a command with a fixed environment. Its standard error goes to
// the terminal, so long steps show progress, and its last lines become the
// error message.
func run(ctx context.Context, c install.Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = append([]string{"PATH=/usr/sbin:/usr/bin", "LC_ALL=C.UTF-8", "HOME=/root"}, c.Env...)
	cmd.Stdin = strings.NewReader(c.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if n := len(lines); n > 3 {
			lines = lines[n-3:]
		}
		if msg := strings.TrimSpace(strings.Join(lines, " ")); msg != "" {
			return stdout.String(), errors.New(msg)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

func quiet(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// bootDisks returns the disks that hold the running system: the live USB,
// its persistence partition, or the root of an installed system.
func bootDisks(ctx context.Context) []string {
	var list []string
	for _, mnt := range []string{"/run/initramfs/live", "/sysroot", "/boot", "/var/lib/sibyl", "/"} {
		src, err := quiet(ctx, "findmnt", "-n", "-o", "SOURCE", mnt)
		if err != nil {
			continue
		}
		src = strings.TrimSpace(src)
		if i := strings.Index(src, "["); i > 0 {
			src = src[:i] // /dev/vda3[/ostree/deploy/...]
		}
		chain, err := quiet(ctx, "lsblk", "-nrps", "-o", "PATH,TYPE", src)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(chain, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[1] == "disk" && !slices.Contains(list, f[0]) {
				list = append(list, f[0])
			}
		}
	}
	return list
}

func listDisks(ctx context.Context) ([]disks.Disk, error) {
	out, err := quiet(ctx, "lsblk", disks.LsblkArgs...)
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	return disks.Parse([]byte(out), bootDisks(ctx), install.MinDiskSize)
}

// images returns the image to install from local storage and the registry
// image that the installed system follows for updates.
func images() (payload, target string, err error) {
	tag, err1 := os.ReadFile("/usr/share/sibyl/image-tag")
	variant, err2 := os.ReadFile("/usr/share/sibyl/gpu-variant")
	if err := errors.Join(err1, err2); err != nil {
		return "", "", err
	}
	v := strings.TrimSpace(string(variant))
	return fmt.Sprintf("%s:%s-%s", registry, strings.TrimSpace(string(tag)), v), fmt.Sprintf("%s:latest-%s", registry, v), nil
}

func hasTPM() bool {
	_, err := os.Stat("/sys/class/tpm/tpm0")
	return err == nil
}

// unattended is the -config file.
type unattended struct {
	Disk       string        `json:"disk"`
	Passphrase string        `json:"passphrase,omitempty"`
	Admin      install.Admin `json:"admin"`
	Password   string        `json:"password,omitempty"` // hashed if admin.password_hash is empty
}

func installCmd(ctx context.Context, config string) error {
	if os.Geteuid() != 0 {
		return errors.New("run it as root")
	}
	list, err := listDisks(ctx)
	if err != nil {
		return err
	}
	payload, target, err := images()
	if err != nil {
		return err
	}
	o := install.Options{Payload: payload, Target: target, TPM: hasTPM()}

	if config != "" {
		b, err := os.ReadFile(config)
		if err != nil {
			return err
		}
		var u unattended
		if err := json.Unmarshal(b, &u); err != nil {
			return fmt.Errorf("%s: %w", config, err)
		}
		i := slices.IndexFunc(list, func(d disks.Disk) bool { return d.Path == u.Disk })
		if i < 0 {
			return fmt.Errorf("disk %s not found", u.Disk)
		}
		o.Disk, o.Passphrase, o.Admin = list[i], u.Passphrase, u.Admin
		if o.Admin.PasswordHash == "" && u.Password != "" {
			if o.Admin.PasswordHash, err = hashPassword(ctx, u.Password); err != nil {
				return err
			}
		}
	} else if err := ask(ctx, list, &o); err != nil {
		return err
	}

	if err := exec.CommandContext(ctx, "podman", "image", "exists", payload).Run(); err != nil {
		fmt.Printf("%s is not on this USB. It is downloaded now, which needs a network connection.\n", payload)
	}

	work, err := os.MkdirTemp("/run", "sibyl-install-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	in := &install.Installer{Run: run, Work: work, Step: func(s string) { fmt.Printf("\n==> %s\n", s) },
		Debug: os.Getenv("SIBYL_INSTALL_DEBUG") == "1"}
	start := time.Now()
	res, err := in.Install(ctx, o)
	if err != nil {
		return err
	}
	fmt.Printf("\nSibyl is installed on %s in %s.\n\n", o.Disk.Path, time.Since(start).Round(time.Second))
	fmt.Println("Recovery key for the disk. Write it down and keep it away from this machine.")
	fmt.Println("You need it if the TPM2 refuses to unlock the disk, for example after a firmware")
	fmt.Println("update or a change to Secure Boot.")
	fmt.Printf("\n    %s\n\n", res.RecoveryKey)
	fmt.Printf("After the reboot, log in as %s and run: sibyl setup\n\n", o.Admin.Name)
	if config == "" {
		var ok bool
		_ = huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("I wrote down the recovery key").
			Affirmative("Reboot now").Negative("Stay in the live system").Value(&ok))).Run()
		if ok {
			return exec.CommandContext(ctx, "systemctl", "reboot").Run()
		}
	}
	return nil
}

func ask(ctx context.Context, list []disks.Disk, o *install.Options) error {
	var opts []huh.Option[string]
	for _, d := range list {
		if d.Reason == "" {
			opts = append(opts, huh.NewOption(d.Label(), d.Path))
		}
	}
	if len(opts) == 0 {
		var b strings.Builder
		for _, d := range list {
			fmt.Fprintf(&b, "\n  %s: %s", d.Label(), d.Reason)
		}
		return errors.New("no disk can take the install:" + b.String())
	}
	var disk, name, pass, pass2, keys, github, passphrase, confirm string
	name = "admin"
	tpmNote := "This machine has a TPM2. The disk unlocks by itself at boot while Secure Boot\n" +
		"stays the same (PCR 7). A passphrase is optional."
	if !o.TPM {
		tpmNote = "This machine has no TPM2. The disk needs a passphrase at every boot."
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().Title("Install Sibyl").Description("The installer erases one disk and installs Sibyl on it."),
			huh.NewSelect[string]().Title("Disk").Options(opts...).Value(&disk),
		),
		huh.NewGroup(
			huh.NewInput().Title("Administrator user name").Description("This account can use sudo.").Value(&name).
				Validate(func(s string) error { return install.Admin{Name: s, PasswordHash: "$6$x"}.Validate() }),
			huh.NewInput().Title("Password").Description("For the console and for sudo. At least 8 characters.").
				EchoMode(huh.EchoModePassword).Value(&pass).
				Validate(func(s string) error {
					if len(s) < 8 {
						return errors.New("use at least 8 characters")
					}
					return nil
				}),
			huh.NewInput().Title("Password again").EchoMode(huh.EchoModePassword).Value(&pass2).
				Validate(func(s string) error {
					if s != pass {
						return errors.New("the passwords differ")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewText().Title("SSH public keys").Description("Optional. One per line. SSH accepts keys only.").Lines(3).Value(&keys).
				Validate(func(s string) error {
					for _, l := range splitLines(s) {
						if _, err := hostd.ParseKey(l); err != nil {
							return err
						}
					}
					return nil
				}),
			huh.NewInput().Title("GitHub user").Description("Optional. Adds the keys from github.com/<user>.keys. Needs a network.").Value(&github),
		),
		huh.NewGroup(
			huh.NewNote().Title("Disk encryption").Description(tpmNote),
			huh.NewInput().Title("Disk passphrase").EchoMode(huh.EchoModePassword).Value(&passphrase).
				Validate(func(s string) error {
					if (s == "" && !o.TPM) || (s != "" && len(s) < 12) {
						return errors.New("use at least 12 characters")
					}
					return nil
				}),
		),
	)
	if err := form.Run(); err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(d disks.Disk) bool { return d.Path == disk })
	o.Disk, o.Passphrase = list[i], passphrase
	o.Admin.Name = name
	o.Admin.Keys = splitLines(keys)
	if github = strings.TrimSpace(github); github != "" {
		k, err := hostd.GitHubKeys(ctx, "https://github.com", github)
		if err != nil {
			return err
		}
		o.Admin.Keys = append(o.Admin.Keys, k...)
	}
	var err error
	if o.Admin.PasswordHash, err = hashPassword(ctx, pass); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}

	want := o.Disk.Serial
	if want == "" {
		want = "erase"
	}
	err = huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Erase "+o.Disk.Path+"?").Description(
			"Everything on this disk is deleted:\n\n  "+o.Disk.Label()+"\n\nType "+want+" to continue."),
		huh.NewInput().Value(&confirm).Validate(func(s string) error {
			if strings.TrimSpace(s) != want {
				return fmt.Errorf("type %s, or press ctrl+c to stop", want)
			}
			return nil
		}),
	)).Run()
	return err
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func hashPassword(ctx context.Context, pass string) (string, error) {
	cmd := exec.CommandContext(ctx, "openssl", "passwd", "-6", "-stdin")
	cmd.Stdin = strings.NewReader(pass + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("hash the password: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// persistCmd adds the persistence partition to the USB that the live system
// runs from.
func persistCmd(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run it as root")
	}
	src, err := quiet(ctx, "findmnt", "-n", "-o", "SOURCE", "/run/initramfs/live")
	if err != nil {
		return install.ErrNotLive
	}
	chain, err := quiet(ctx, "lsblk", "-nrps", "-o", "PATH,TYPE,SIZE", "-b", strings.TrimSpace(src))
	if err != nil {
		return err
	}
	var disk string
	var size int64
	for _, line := range strings.Split(chain, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[1] == "disk" {
			disk = f[0]
			_, _ = fmt.Sscan(f[2], &size)
		}
	}
	if disk == "" {
		return install.ErrNotLive
	}
	ok := false
	if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
		Title("Make a persistence partition on " + disk + "?").
		Description("It uses the free space after the live system. The live system stays bootable.\n" +
			"The Sibyl services, models and settings are then kept on the USB.").
		Affirmative("Make it").Negative("Cancel").Value(&ok))).Run(); err != nil || !ok {
		return err
	}
	part, err := install.Persist(ctx, run, disk, size)
	if err != nil {
		return err
	}
	fmt.Printf("Made %s. Reboot to start the Sibyl services from it.\n", part)
	return nil
}
