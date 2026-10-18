//go:build linux

package main

import (
	"math"

	"golang.org/x/sys/unix"
)

// close_range (Linux 5.11+) reaches every descriptor, even with no /proc and above the sweep.
func closeRangeOnExec() bool {
	return unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC) == nil
}
