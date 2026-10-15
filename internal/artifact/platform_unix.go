//go:build !windows

package artifact

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// beforeDirectoryChmod runs between the ownership check and the chmod of a
// directory; tests use it to swap the directory for a symlink in that window.
var beforeDirectoryChmod func(path string)

func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("artifact directory %s is not a directory", path)
	}
	return nil
}

func makePrivateDirectory(path string) error {
	return makePrivateDirectoryForUID(path, uint32(os.Geteuid()))
}

// makePrivateDirectoryForUID checks and tightens the directory through one
// handle, so the directory that was checked is the one that is chmodded, and
// then confirms the path still names that directory.
func makePrivateDirectoryForUID(path string, uid uint32) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("artifact directory %s is not a directory", path)
	}
	if stat.Uid != uid {
		return fmt.Errorf("artifact directory %s is not owned by the current user", path)
	}
	if beforeDirectoryChmod != nil {
		beforeDirectoryChmod(path)
	}
	if stat.Mode&0o777 != 0o700 {
		if err := syscall.Fchmod(fd, 0o700); err != nil {
			return &os.PathError{Op: "fchmod", Path: path, Err: err}
		}
	}
	var now syscall.Stat_t
	if err := syscall.Lstat(path, &now); err != nil {
		return &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	if now.Dev != stat.Dev || now.Ino != stat.Ino {
		return fmt.Errorf("artifact directory %s was replaced while being made private", path)
	}
	return nil
}

func openBlob(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	return errors.Join(err, closeErr)
}

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// A reader never blocks a replacement on Unix, so nothing is classified busy.
func isBusy(error) bool { return false }

func replaceError(err error, _ string) error { return err }
