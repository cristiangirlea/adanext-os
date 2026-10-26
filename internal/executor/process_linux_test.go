package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTreeSignalsGoOnlyToProcessesStillTheOnesScanned(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
	child.Env = append(os.Environ(), "ADA_FAKE_ENGINE=grandchild_hold", "ADA_FAKE_MARKER="+filepath.Join(t.TempDir(), "marker"))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	self, err := readProcessRecord(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	other, err := readProcessRecord(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Recorded, never sent: these are the test's own group.
	var sent []int
	record := func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGSTOP {
			t.Errorf("signal %v, want SIGSTOP", sig)
		}
		sent = append(sent, pid)
		return nil
	}
	signalGroups(record, map[int]processRecord{self.pid: self, other.pid: other}, syscall.SIGSTOP)
	if len(sent) != 1 || sent[0] != -self.group {
		t.Fatalf("one group of two members got %v, want [%d]", sent, -self.group)
	}
	sent = nil
	self.created = "reused"
	signalGroups(record, map[int]processRecord{self.pid: self}, syscall.SIGSTOP)
	if len(sent) != 0 {
		t.Fatalf("a process with another creation time got %v", sent)
	}
}

func TestABackendGroupWhoseLeaderExitedIsStillKilled(t *testing.T) {
	cmd, s, marker, waited := startSupervised(t, "orphaned_group")
	cleanupGrandchild(t, marker)
	// The descendant has been reparented away from the engine, and no process
	// leads its group any more.
	waitMarker(t, marker, "leader exited")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	var identity string
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &identity); err == nil {
			break
		}
	}
	if pid == 0 {
		t.Fatalf("descendant did not record itself: %s", data)
	}
	if err := s.kill(cmd); err != nil {
		t.Fatalf("kill = %v", err)
	}
	<-waited
	if err := waitProcessGone(pid, identity, 10*time.Second); err != nil {
		t.Fatalf("descendant in a leaderless group survived the kill: %v", err)
	}
}

func TestASupervisorTakesNoProcessForSharingItsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	hold := func(marker string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", marker)
		cmd.Dir = dir
		return cmd
	}
	// Unrelated: in the test's session, a group of its own, working in dir.
	unrelated := hold(filepath.Join(t.TempDir(), "unrelated"))
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	root := hold(filepath.Join(t.TempDir(), "root"))
	s, err := newProcessSupervisor(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Process.Kill(); _ = root.Wait() })
	if err := s.started(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.signal = syscall.Kill; s.close() })
	// Recorded, never sent.
	var sent []int
	s.signal = func(pid int, sig syscall.Signal) error { sent = append(sent, pid); return nil }
	tree := s.stopAll()
	for _, pid := range []int{unrelated.Process.Pid, os.Getpid()} {
		if _, ok := tree[pid]; ok {
			t.Errorf("the tree took %d, which only shares the working directory or its session", pid)
		}
	}
	if _, ok := tree[root.Process.Pid]; !ok {
		t.Errorf("the tree misses its root: %v", tree)
	}
	self, _ := readProcessRecord(os.Getpid())
	for _, pid := range sent {
		if pid == -unrelated.Process.Pid || pid == -self.group {
			t.Errorf("signalled group %d", -pid)
		}
	}
}

func TestWaitTreeReapsExitedOrphansThoughAMemberOutlivesItsDeadline(t *testing.T) {
	stuck := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", filepath.Join(t.TempDir(), "stuck"))
	if err := stuck.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stuck.Process.Kill(); _ = stuck.Wait() })
	s := &processSupervisor{}
	// Members are visited in map order: over three rounds the stuck one comes
	// before some exited one, which only a pass after the deadline reaps.
	for round := range 3 {
		record, err := readProcessRecord(stuck.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		tree := map[int]processRecord{record.pid: record}
		var exitedPids []int
		for range 8 {
			child := exec.Command(os.Args[0], "-test.run=^$")
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			pid := child.Process.Pid
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				r, err := readProcessRecord(pid)
				if err != nil {
					t.Fatal(err)
				}
				if exited(r) {
					tree[pid] = r
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("child %d never exited", pid)
				}
			}
			exitedPids = append(exitedPids, pid)
		}
		s.waitTreeWithin(tree, 20*time.Millisecond)
		for _, pid := range exitedPids {
			if r, err := readProcessRecord(pid); err == nil {
				t.Fatalf("round %d: exited member %d left unreaped (%s)", round, pid, r.state)
			}
		}
	}
}

func TestAFailedSubreaperCallIsReported(t *testing.T) {
	failing := func(int, uintptr, uintptr, uintptr, uintptr) error { return errors.New("refused") }
	if err := subreaper(failing); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("subreaper = %v, want the prctl error", err)
	}
}
