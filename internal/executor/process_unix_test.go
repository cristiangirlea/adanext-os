//go:build !windows

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// newBackendSupervisor mirrors Harness on Unix: a backend in its own process
// group, inside the engine's session.
func newBackendSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processSupervisor{signal: syscall.Kill}, nil
}

// startSupervised starts the fake engine under a supervisor, as run does.
func startSupervised(t *testing.T, script string) (*exec.Cmd, *processSupervisor, string, <-chan struct{}) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "marker")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
	cmd.Env = append(os.Environ(), "ADA_FAKE_ENGINE="+script, "ADA_FAKE_MARKER="+marker)
	s, err := newProcessSupervisor(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.started(cmd); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	return cmd, s, marker, waited
}

func TestAFailedGroupKillFallsBackToKillingTheEngineItself(t *testing.T) {
	cmd, s, marker, waited := startSupervised(t, "grandchild_hold")
	cleanupGrandchild(t, marker)
	waitMarker(t, marker, "child:")
	// The engine has left its group, say: every group signal fails.
	s.signal = func(pid int, sig syscall.Signal) error {
		if pid < 0 {
			return syscall.ESRCH
		}
		return syscall.Kill(pid, sig)
	}
	if err := s.kill(cmd); err != nil {
		t.Fatalf("kill = %v", err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine survived a kill whose group signal failed")
	}
}

func TestAKillOfAPidNoLongerTheEngineSignalsNoGroup(t *testing.T) {
	cmd, s, marker, waited := startSupervised(t, "grandchild_hold")
	cleanupGrandchild(t, marker)
	waitMarker(t, marker, "child:")
	// The pid now names another process, say: its group is not the engine's.
	s.created = "reused"
	var sent []int
	s.signal = func(pid int, sig syscall.Signal) error {
		sent = append(sent, pid)
		return nil
	}
	if err := s.kill(cmd); err != nil {
		t.Fatalf("kill = %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("signals sent by pid alone: %v", sent)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine survived the fallback kill")
	}
}
