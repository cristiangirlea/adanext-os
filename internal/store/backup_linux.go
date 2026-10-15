package store

import "golang.org/x/sys/unix"

// renameNoReplace renames oldPath to newPath, failing with EEXIST when newPath
// exists. Kernels before 3.15 and some filesystems refuse the flag.
func renameNoReplace(oldPath, newPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
}
