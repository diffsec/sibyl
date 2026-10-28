// Command sibyl-update applies an offline update bundle.
//
//	sibyl-update verify <dir>   check the signature and every archive
//	sibyl-update apply <dir>    verify, load the images, and stage the OS
//
// A bundle comes from CI (scripts/bundle/make.sh). Copy its directory to a
// USB drive, mount it on the host, and run apply as root. Reboot afterwards.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/ihavespoons/sibyl/internal/bundle"
)

const (
	keyPath = "/etc/pki/containers/sibyl-cosign.pub"
	stage   = "/var/lib/sibyl-update"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sibyl-update verify|apply <bundle directory>")
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, flag.Arg(0), flag.Arg(1)); err != nil {
		fmt.Fprintln(os.Stderr, "sibyl-update:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd, dir string) error {
	key, err := bundle.LoadPublicKey(keyPath)
	if err != nil {
		return err
	}
	fmt.Println("Checking the signature and every archive. This reads the whole bundle.")
	m, err := bundle.Verify(dir, key, func(im bundle.Image) {
		fmt.Printf("  %s (%.1f GB)\n", im.Ref, float64(im.Size)/1e9)
	})
	if err != nil {
		return err
	}
	fmt.Printf("The bundle for %s (%s) is signed by the project key and complete.\n", m.Tag, m.Variant)
	if b, err := os.ReadFile("/usr/share/sibyl/gpu-variant"); err == nil {
		if v := strings.TrimSpace(string(b)); v != m.Variant {
			return fmt.Errorf("this bundle is for the %s variant, and this host runs %s", m.Variant, v)
		}
	}
	switch cmd {
	case "verify":
		return nil
	case "apply":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if os.Geteuid() != 0 {
		return errors.New("apply must run as root")
	}
	u, err := user.Lookup("sibyl")
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	a := &bundle.Applier{Run: runCmd, Stage: stage, SibylUID: uid, SibylGID: gid,
		Step: func(s string) { fmt.Println("==> " + s) }}
	if err := a.Apply(ctx, dir, m); err != nil {
		return err
	}
	fmt.Printf("\nThe update to %s is staged. Reboot to start it: sudo systemctl reboot\n", m.Tag)
	fmt.Printf("bootc now follows the archive. When the host is online again, return to the registry with:\n")
	fmt.Printf("  sudo bootc switch --enforce-container-sigpolicy ghcr.io/ihavespoons/sibyl-os:latest-%s\n", m.Variant)
	return nil
}

// runCmd runs a command from / with its output on the terminal.
func runCmd(ctx context.Context, c bundle.Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
