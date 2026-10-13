//go:build !windows

package artifact

import "syscall"

func injectedDiskFull() error { return syscall.ENOSPC }
