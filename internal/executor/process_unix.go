//go:build !windows

package executor

import (
	"os/exec"
	"sync"
	"syscall"
)

type processRecord struct {
	pid, parent, group, session int
	created                     string
	state                       string
}

type processSupervisor struct {
	pid     int
	created string                                  // The engine's creation time: a reused pid is never taken for it.
	signal  func(pid int, sig syscall.Signal) error // Test seam for the signals sent.
	mu      sync.Mutex
	seen    map[int]processRecord
	// sessions maps the leader of each session a real descendant started to
	// the leader's creation time, so a reused session id is never taken.
	sessions map[int]string
	closed   bool
}

// supervisors are the live supervisors of this process, from creation to
// close: an orphan reparented to adad is claimed only when no other could own it.
var (
	supervisorsMu sync.Mutex
	supervisors   = map[*processSupervisor]bool{}
)

func newProcessSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	becomeSubreaper()
	// A session of its own: descendants keep its id when they are reparented
	// or their group leader exits, and Harness's backend groups stay in it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	s := &processSupervisor{
		signal:   syscall.Kill,
		seen:     map[int]processRecord{},
		sessions: map[int]string{},
	}
	supervisorsMu.Lock()
	supervisors[s] = true
	supervisorsMu.Unlock()
	return s, nil
}

func (s *processSupervisor) started(cmd *exec.Cmd) error {
	created := processCreated(cmd.Process.Pid)
	supervisorsMu.Lock()
	s.pid, s.created = cmd.Process.Pid, created
	supervisorsMu.Unlock()
	return nil
}

// close kills what is left of the engine's tree when the attempt ends, as
// closing the Job Object does on Windows. A second close does nothing.
func (s *processSupervisor) close() {
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if !closed && s.pid != 0 {
		tree := s.stopAll()
		killTree(s.signal, tree)
		s.waitTree(tree)
	}
	supervisorsMu.Lock()
	delete(supervisors, s)
	supervisorsMu.Unlock()
}

func (s *processSupervisor) kill(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	if processCreated(pid) != s.created {
		// Reaped, the pid may name another process: only the engine's own
		// handle is safe, and close still ends what the engine left.
		return cmd.Process.Kill()
	}
	// Stopped first, the engine forks nothing while its tree is scanned.
	_ = s.signal(-pid, syscall.SIGSTOP)
	tree := s.stopAll()
	err := s.signal(-pid, syscall.SIGKILL)
	killTree(s.signal, tree)
	if err != nil {
		// A session leader cannot leave its group, so the engine was reaped
		// since the check: its own handle reports that.
		err = cmd.Process.Kill()
	}
	return err
}
