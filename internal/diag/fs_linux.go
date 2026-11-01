package diag

import "golang.org/x/sys/unix"

func setLabel(path, label string) error {
	return unix.Lsetxattr(path, "security.selinux", append([]byte(label), 0), 0)
}
