//go:build linux

package main

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Without /proc and its /dev/fd, the sweep stops at 65536; close_range covers every number.
func TestCloseRangeMarksADescriptorAboveTheSweepWithoutAListing(t *testing.T) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	const high = 70000
	if limit.Cur <= high {
		raised := limit
		raised.Cur = min(limit.Max, high+1)
		if raised.Cur <= high || syscall.Setrlimit(syscall.RLIMIT_NOFILE, &raised) != nil {
			t.Skipf("RLIMIT_NOFILE %d cannot reach descriptor %d", limit.Max, high)
		}
		defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)
	}
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	defer unix.Close(pipe[1])
	// Inheritable, as a descriptor Electron hands over is.
	if err := unix.Dup3(pipe[1], high, 0); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(high)
	markInheritedOnExec("/nonexistent-descriptor-listing", closeRangeOnExec, syscall.CloseOnExec)
	flags, err := unix.FcntlInt(uintptr(high), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor %d stays inheritable", high)
	}
}
