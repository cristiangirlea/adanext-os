//go:build windows

package artifact

import "golang.org/x/sys/windows"

func injectedDiskFull() error { return windows.ERROR_DISK_FULL }
