//go:build !windows

package ipc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func tryLockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

func isLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func validateStateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkStateDirInfo(path, info)
}

func checkStateDirInfo(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || info.Mode().Perm()&0022 != 0 || stat.Uid != uint32(os.Getuid()) {
		return stateDirError(path)
	}
	return nil
}

func stateDirError(path string) error {
	return fmt.Errorf("ipc: state directory %q must be a real directory owned by the user and not group- or other-writable", path)
}

// lockDir is the checked state directory, held open so the lock file is
// opened relative to the directory that was checked rather than by path.
type lockDir struct {
	dir *os.File
}

func openLockDir(path string) (*lockDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	// A symlink refused by O_NOFOLLOW is ELOOP on Linux and macOS, EMLINK on FreeBSD.
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) || errors.Is(err, unix.ENOTDIR) {
		return nil, stateDirError(path)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	dir := os.NewFile(uintptr(fd), path)
	info, err := dir.Stat()
	if err == nil {
		err = checkStateDirInfo(path, info)
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return &lockDir{dir: dir}, nil
}

func (d *lockDir) openLock() (*os.File, error) {
	dirfd := int(d.dir.Fd())
	fd, err := unix.Openat(dirfd, lockFileName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(dirfd, lockFileName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	path := filepath.Join(d.dir.Name(), lockFileName)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (d *lockDir) Close() error { return d.dir.Close() }

func validateLockFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("ipc: lock file %q must be a regular owner-only file", f.Name())
	}
	return nil
}

func retryStaleLock(*os.File, lockRecord) (bool, error) { return false, nil }
