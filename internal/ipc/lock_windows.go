//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"os"

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
