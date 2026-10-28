package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provision creates the admin account that the installer recorded, at the
// first boot of the installed system, and then deletes the record. stateVar
// is /var. It does nothing when there is no record.
func Provision(ctx context.Context, run Runner, stateVar string) (string, error) {
	path := filepath.Join(stateVar, ProvisionFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var a Admin
	if err := json.Unmarshal(data, &a); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if err := a.Validate(); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if out, _ := run(ctx, Cmd{Name: "getent", Args: []string{"passwd", a.Name}}); strings.TrimSpace(out) == "" {
		if _, err := run(ctx, Cmd{Name: "useradd", Args: []string{"--create-home", "--user-group",
			"--groups", "wheel,sibyl-operators", "--password", a.PasswordHash, "--comment", "Sibyl administrator", a.Name}}); err != nil {
			return "", err
		}
	}
	if len(a.Keys) > 0 {
		out, err := run(ctx, Cmd{Name: "getent", Args: []string{"passwd", a.Name}})
		f := strings.Split(strings.TrimSpace(out), ":")
		if err != nil || len(f) != 7 || !filepath.IsAbs(f[5]) {
			return "", fmt.Errorf("find the home directory of %s: %v", a.Name, err)
		}
		home := f[5]
		// runuser -u, not -l: the -l PAM stack opens a logind session, which
		// fails this early in boot and leaves a failed session scope.
		const script = `umask 077 && mkdir -p "$1/.ssh" && cat > "$1/.ssh/authorized_keys"`
		if _, err := run(ctx, Cmd{Name: "runuser", Args: []string{"-u", a.Name, "--", "/bin/sh", "-c", script, "sh", home},
			Stdin: strings.Join(a.Keys, "\n") + "\n"}); err != nil {
			return "", fmt.Errorf("write the SSH keys: %w", err)
		}
		if _, err := run(ctx, Cmd{Name: "restorecon", Args: []string{"-R", filepath.Join(home, ".ssh")}}); err != nil {
			return "", err
		}
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return "", err
	}
	return fmt.Sprintf("created administrator %s with %d SSH keys", a.Name, len(a.Keys)), nil
}
