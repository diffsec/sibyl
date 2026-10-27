//go:build !linux

package hostd

import (
	"io"
	"os"
	"path/filepath"
	"sort"
)

// readQueue is the portable form for tests on other systems. sibyl-hostd
// runs on Linux only, where fs_linux.go also refuses symlinks.
func readQueue(dir string) ([]queued, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []queued
	for _, name := range names {
		p := filepath.Join(dir, name)
		q := queued{name: name}
		if f, err := os.Open(p); err == nil {
			q.data, q.err = io.ReadAll(io.LimitReader(f, MaxRequestSize))
			_ = f.Close()
		} else {
			q.err = err
		}
		_ = os.Remove(p)
		out = append(out, q)
	}
	return out, nil
}

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

func setLabel(string, string) error { return nil }
