package store

import "golang.org/x/sys/unix"

// renameNoReplace renames oldPath to newPath, failing with EEXIST when newPath
// exists. Filesystems without the flag refuse it with ENOTSUP.
func renameNoReplace(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}
