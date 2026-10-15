//go:build !windows

package executor

import (
	"os/exec"
	"syscall"
)

type processSupervisor struct{}

func newProcessSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processSupervisor{}, nil
}

func (*processSupervisor) started(*exec.Cmd) error { return nil }
func (*processSupervisor) close()                  {}
func (*processSupervisor) kill(cmd *exec.Cmd) error {
	// Harness puts backends in their own groups, so snapshot the tree while
	// the engine still owns its children, before killing its group.
	groups := descendantGroups(cmd.Process.Pid)
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	killDescendantGroups(groups)
	return err
}
