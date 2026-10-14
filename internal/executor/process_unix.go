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
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
