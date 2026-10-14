//go:build windows

package artifact

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

func refuseSymlink(string) error { return nil }
func syncDirectory(string) error { return nil }
func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) || errors.Is(err, syscall.ENOSPC)
}
