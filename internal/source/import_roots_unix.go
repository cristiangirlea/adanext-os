//go:build !windows

package source

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func localImportVolume(string) bool { return true }

func importDirectoryPath(dir *os.File) (string, error) {
	current, err := OpenImportDirectory(dir.Name(), "")
	if err != nil {
		return "", err
	}
	defer current.Close()
	if !SameDirectory(dir, current) {
		return "", errLink
	}
	return dir.Name(), nil
}

func walkImportDirectory(volume string, parts []string, rootDepth int, list, checkPermissions bool, anchor *os.File) (*os.File, error) {
	fd := unix.AT_FDCWD
	var dir *os.File
	name, full := volume, volume
	topDepth := importPermissionTopDepth(volume, parts, rootDepth)
	for depth := 0; ; depth++ {
		access := importSearchAccess
		if list && depth == len(parts) {
			access = unix.O_RDONLY
		}
		next, err := unix.Openat(fd, name, access|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			err = linkError(fd, name, err)
		}
		if dir != nil {
			dir.Close()
		}
		if err != nil {
			if anchor != nil && depth <= rootDepth {
				return nil, errLink
			}
			return nil, err
		}
		fd = next
		dir = os.NewFile(uintptr(fd), full)
		if depth == rootDepth && anchor != nil && !SameDirectory(anchor, dir) {
			dir.Close()
			return nil, errLink
		}
		if checkPermissions && depth >= topDepth {
			if err := importPermissionCheck(dir, depth < rootDepth, depth == topDepth); err != nil {
				dir.Close()
				return nil, err
			}
		}
		if depth == len(parts) {
			return dir, nil
		}
		name = parts[depth]
		full = filepath.Join(full, name)
	}
}

func checkImportPermissions(dir *os.File, ancestor, _ bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return err
	}
	if (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) ||
		(st.Mode&0o022 != 0 && (!ancestor || st.Mode&unix.S_ISVTX == 0)) {
		return writableImportRoot()
	}
	return nil
}
