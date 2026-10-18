//go:build !windows

package source

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// createUnder creates the new file rel under root with content, opening each
// directory on its path relative to its parent's descriptor and refusing a
// link anywhere on it, so a link in the tree cannot redirect the write.
func createUnder(root, rel string, executable bool, content []byte) error {
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: linkError(unix.AT_FDCWD, root, err)}
	}
	defer func() { unix.Close(dir) }()
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		if err := unix.Mkdirat(dir, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: rel, Err: err}
		}
		next, err := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return &os.PathError{Op: "open", Path: rel, Err: linkError(dir, part, err)}
		}
		unix.Close(dir)
		dir = next
	}
	perm := uint32(0o644)
	if executable {
		perm = 0o755
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return &os.PathError{Op: "create", Path: rel, Err: linkError(dir, parts[len(parts)-1], err)}
	}
	f := os.NewFile(uintptr(fd), rel)
	// The umask may have cleared bits the mode needs.
	if err := f.Chmod(os.FileMode(perm)); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// linkError reports O_NOFOLLOW's refusal of the link name in dir as errLink:
// ELOOP, EMLINK on the BSDs, or ENOTDIR where O_DIRECTORY is checked first.
func linkError(dir int, name string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
		return errLink
	}
	var st unix.Stat_t
	if errors.Is(err, unix.ENOTDIR) && unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errLink
	}
	return err
}
