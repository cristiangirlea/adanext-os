//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func lockOffset() windows.Overlapped { return windows.Overlapped{OffsetHigh: 1} }

func tryLockFile(f *os.File) error {
	o := lockOffset()
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
}

func unlockFile(f *os.File) error {
	o := lockOffset()
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o)
}

func isLockContended(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

func validateStateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkStateDirInfo(path, info)
}

// checkStateDirInfo refuses symlinks and every other reparse point, such as
// a junction, which Lstat reports as irregular rather than as a symlink.
func checkStateDirInfo(path string, info os.FileInfo) error {
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("ipc: state directory %q must be a real directory", path)
	}
	return nil
}

// lockDir is the checked state directory; Windows has no openat, so the lock
// is opened by path without following reparse points.
type lockDir struct {
	path string
}

func openLockDir(path string) (*lockDir, error) {
	if err := validateStateDir(path); err != nil {
		return nil, err
	}
	return &lockDir{path: path}, nil
}

func (d *lockDir) openLock() (*os.File, error) {
	path := filepath.Join(d.path, lockFileName)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	h, err := windows.CreateFile(name, access, share, nil, windows.CREATE_NEW, attrs, 0)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) {
		h, err = windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING, attrs, 0)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func (d *lockDir) Close() error { return nil }

func validateLockFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ipc: lock file %q is not regular", f.Name())
	}
	return nil
}

// retryStaleLockEntered is a test hook signalled on entry to retryStaleLock;
// it is nil outside tests.
var retryStaleLockEntered func()

func retryStaleLock(f *os.File, record lockRecord) (bool, error) {
	if retryStaleLockEntered != nil {
		retryStaleLockEntered()
	}
	if record.PID <= 0 {
		return false, nil
	}
	created, err := processCreated(record.PID)
	if err == nil && created == record.Created {
		return false, nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if err := tryLockFile(f); err == nil {
			return true, nil
		} else if !isLockContended(err) {
			return false, err
		}
	}
	return false, nil
}

func processCreated(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)), nil
}
