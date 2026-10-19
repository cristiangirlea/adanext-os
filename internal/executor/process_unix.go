//go:build !windows

package executor

import (
	"os/exec"
	"syscall"
)

type processRecord struct {
	pid, parent, group, session int
	created                     string
}

type processSupervisor struct {
	pid     int
	created string                                  // The engine's creation time: a reused pid is never taken for it.
	signal  func(pid int, sig syscall.Signal) error // Test seam for the signals sent.
}

func newProcessSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	// A session of its own: descendants keep its id when they are reparented
	// or their group leader exits, and Harness's backend groups stay in it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return &processSupervisor{signal: syscall.Kill}, nil
}

func (s *processSupervisor) started(cmd *exec.Cmd) error {
	s.pid, s.created = cmd.Process.Pid, processCreated(cmd.Process.Pid)
	return nil
}

// close kills what is left of the engine's tree when the attempt ends, as
// closing the Job Object does on Windows.
func (s *processSupervisor) close() {
	if s.pid != 0 {
		killTree(s.signal, stopTree(s.signal, s.pid, s.created))
	}
}

func (s *processSupervisor) kill(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	// Stopped first, the engine forks nothing while its tree is scanned.
	_ = s.signal(-pid, syscall.SIGSTOP)
	tree := stopTree(s.signal, pid, s.created)
	err := s.signal(-pid, syscall.SIGKILL)
	killTree(s.signal, tree)
	if err != nil {
		// The group kill failed (the engine left its group): kill the engine itself.
		err = cmd.Process.Kill()
	}
	return err
}
