//go:build linux

package hostd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadSecretRefusesSymlinks checks that root never reads a file that
// the sibyl user points the secret at.
func TestReadSecretRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(target, []byte("root:secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "1.key")); err != nil {
		t.Fatal(err)
	}
	if b, err := readSecret(dir, "1.key"); err == nil {
		t.Errorf("read through a symlink: %q", b)
	}
	link := filepath.Join(t.TempDir(), "secrets")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.key"), []byte("tskey-auth-x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(link, "2.key"); err == nil {
		t.Error("read through a symlinked directory")
	}
	b, err := readSecret(dir, "2.key")
	if err != nil || string(b) != "tskey-auth-x" {
		t.Errorf("a plain file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.key")); !os.IsNotExist(err) {
		t.Error("the secret stayed")
	}
}
