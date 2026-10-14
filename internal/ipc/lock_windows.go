//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"os"
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
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("ipc: state directory %q must be a real directory", path)
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return os.OpenFile(path, os.O_RDWR, 0)
	}
	return f, err
}

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

func retryStaleLock(f *os.File, record lockRecord) (bool, error) {
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
