package bundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Cmd is one command to run as root.
type Cmd struct {
	Name string
	Args []string
}

func (c Cmd) String() string { return fmt.Sprint(append([]string{c.Name}, c.Args...)) }

// Runner runs a command.
type Runner func(ctx context.Context, c Cmd) error

// Applier loads a verified bundle onto the host.
type Applier struct {
	Run Runner
	// Stage is a directory that root owns and the sibyl group can read. The
	// sibyl user's skopeo reads each archive from here, because the bundle
	// itself may be on a USB drive that sibyl cannot read.
	Stage string
	// SibylUID is the sibyl user's ID, for its XDG_RUNTIME_DIR.
	SibylUID int
	// SibylGID owns the staged archives, so the sibyl user can read them.
	SibylGID int
	// Step reports progress.
	Step func(string)
}

func (a *Applier) step(format string, args ...any) {
	if a.Step != nil {
		a.Step(fmt.Sprintf(format, args...))
	}
}

// Apply loads every image of a verified bundle in dir. The service images
// go into the sibyl user's storage with their digests unchanged, so the
// Quadlet units find the images that they pin. The TUI archive is kept for
// the sibyl command, and bootc stages the OS image for the next boot.
func (a *Applier) Apply(ctx context.Context, dir string, m Manifest) error {
	if err := os.MkdirAll(a.Stage, 0o750); err != nil {
		return err
	}
	// The process runs as root, so root owns what it creates. Only the group changes.
	if err := os.Chown(a.Stage, -1, a.SibylGID); err != nil {
		return err
	}
	if err := os.Chmod(a.Stage, 0o750); err != nil {
		return err
	}
	home := "/var/lib/sibyl/home"
	runtime := fmt.Sprintf("/run/user/%d", a.SibylUID)
	for _, im := range m.Images {
		src := filepath.Join(dir, im.File)
		switch im.Store {
		case Sibyl:
			a.step("Loading %s", im.Ref)
			staged := filepath.Join(a.Stage, im.File)
			if err := copyFile(src, staged, 0o640, a.SibylGID); err != nil {
				return err
			}
			// A digest reference is stored under its repository name. The
			// image keeps its digest, which the pinned reference matches.
			dest := im.Ref
			if strings.Contains(im.Ref, "@") {
				dest = RepoName(im.Ref)
			}
			err := a.Run(ctx, Cmd{Name: "runuser", Args: []string{"-u", "sibyl", "--",
				"env", "XDG_RUNTIME_DIR=" + runtime, "HOME=" + home,
				"skopeo", "copy", "--quiet", "--preserve-digests",
				"oci-archive:" + staged, "containers-storage:" + dest}})
			_ = os.Remove(staged)
			if err != nil {
				return fmt.Errorf("load %s: %w", im.Ref, err)
			}
		case TUI:
			a.step("Keeping %s for the sibyl command", im.Ref)
			if err := copyFile(src, filepath.Join(a.Stage, "sibyl-tui.oci.tar"), 0o644, -1); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(a.Stage, "sibyl-tui.ref"), []byte(im.Ref+"\n"), 0o644); err != nil {
				return err
			}
		}
	}
	// The stage must stay readable for operators, who load the TUI from it.
	if err := os.Chmod(a.Stage, 0o755); err != nil {
		return err
	}

	osImage := m.OSImage()
	abs, err := filepath.Abs(filepath.Join(dir, osImage.File))
	if err != nil {
		return err
	}
	a.step("Staging %s for the next boot", osImage.Ref)
	return a.Run(ctx, Cmd{Name: "bootc", Args: []string{"switch", "--enforce-container-sigpolicy",
		"--transport", "oci-archive", abs}})
}

func copyFile(src, dst string, mode os.FileMode, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmp, -1, gid); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
