//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Node's detached spawn gives us a new session, but stdio "ignore" replaces only fds
// 0-2: every other descriptor Electron holds without close-on-exec comes along, such as
// Chromium's and the channels whoever launched Electron reads. Go's os/exec does not
// close such inherited descriptors in its children either. Before opening state, mark
// them all close-on-exec and re-exec: the serving process gets only its own null stdio,
// and this one exits, so nothing the desktop held outlives it.
func detachFromJob(args []string) (bool, error) {
	closeInheritedOnExec()
	return detachWith(args, func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	})
}

// Go opens its own descriptors close-on-exec, so marking them again changes nothing.
func closeInheritedOnExec() {
	if dir, err := os.Open("/dev/fd"); err == nil {
		names, err := dir.Readdirnames(-1)
		dir.Close()
		if err == nil {
			for _, name := range names {
				if fd, err := strconv.Atoi(name); err == nil && fd > 2 {
					syscall.CloseOnExec(fd)
				}
			}
			return
		}
	}
	// Without a descriptor listing, mark every number the limit allows.
	var limit syscall.Rlimit
	last := uint64(1 << 16)
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit) == nil && limit.Cur < last {
		last = limit.Cur
	}
	for fd := 3; uint64(fd) < last; fd++ {
		syscall.CloseOnExec(fd)
	}
}

func detachWith(args []string, start func(*exec.Cmd) error) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	cmd := exec.Command(executable, append(append([]string{}, args...), "-detach=false")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := start(cmd); err != nil {
		return false, fmt.Errorf("cannot detach adad from the desktop: %w", err)
	}
	return true, nil
}
