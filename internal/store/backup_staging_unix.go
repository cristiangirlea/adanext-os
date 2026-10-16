//go:build !windows

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// openBackupLock opens or creates the lock file itself, never a symlink's target.
func openBackupLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	lock := os.NewFile(uintptr(fd), path)
	info, err := lock.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("backup lock %s is not a regular file", path)
	}
	if err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// tryLockBackup takes an flock, which belongs to the open file and so also
// excludes another Store of this process, without waiting for it.
func tryLockBackup(lock *os.File) (bool, error) {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func unlockBackup(lock *os.File) error {
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}

// stagedFile is what the sweep saw of a staging entry, without following it.
type stagedFile struct{ info os.FileInfo }

func inspectStaged(path string) (stagedFile, error) {
	info, err := os.Lstat(path)
	return stagedFile{info}, err
}

func (f stagedFile) regular() bool      { return f.info.Mode().IsRegular() }
func (f stagedFile) modTime() time.Time { return f.info.ModTime() }

// removeStaged unlinks path only while it is still the regular file seen. An
// unlink never follows a symlink and never removes a directory, so whatever
// replaces the file in the last instant is at worst unlinked itself.
func removeStaged(path string, seen stagedFile) error {
	now, err := inspectStaged(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !now.regular() || !os.SameFile(seen.info, now.info) || !now.modTime().Equal(seen.modTime()) {
		return nil
	}
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
