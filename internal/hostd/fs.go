package hostd

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// writeAtomic writes a file through a temporary file and a rename, in a
// directory that only root can write.
func writeAtomic(path string, data []byte, mode os.FileMode, label string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if label != "" {
		if err := setLabel(tmp.Name(), label); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}

// countKeys counts the keys in a home directory's authorized_keys. It does
// not follow a symlink, so a user cannot make root read another file.
func countKeys(home string) int {
	f, err := openNoFollow(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	n := 0
	sc := bufio.NewScanner(io.LimitReader(f, MaxRequestSize))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			n++
		}
	}
	return n
}
