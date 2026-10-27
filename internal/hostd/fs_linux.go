package hostd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

// readQueue reads and removes every file in the queue directory. The
// directory belongs to the sibyl user, so no path component may be a
// symlink, and each file must be a regular file.
func readQueue(dir string) ([]queued, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, dir, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	d := os.NewFile(uintptr(fd), dir)
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []queued
	for _, name := range names {
		q := queued{name: name}
		q.data, q.err = readOne(fd, name)
		if err := unix.Unlinkat(fd, name, 0); err != nil && q.err == nil {
			q.err = fmt.Errorf("remove the request: %w", err)
		}
		out = append(out, q)
	}
	return out, nil
}

func readOne(dirfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("the request is not a regular file")
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("the request is not a regular file")
	}
	if st.Size() > MaxRequestSize {
		return nil, fmt.Errorf("the request is larger than %d bytes", MaxRequestSize)
	}
	return io.ReadAll(io.LimitReader(f, MaxRequestSize))
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

func setLabel(path, label string) error {
	return unix.Lsetxattr(path, "security.selinux", append([]byte(label), 0), 0)
}
