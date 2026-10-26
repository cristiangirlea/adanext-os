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

func TestASupervisorLeavesAnotherTreesOrphanInItsSessionToThatTree(t *testing.T) {
	// The other tree's engine starts a descendant in its session and exits:
	// the descendant is reparented to the test, as to adad, while the other
	// supervisor is still open, waiting for its stderr.
	_, other, otherMarker, otherWaited := startSupervised(t, "group_leader_exits")
	cleanupGrandchild(t, otherMarker)
	<-otherWaited
	var pid int
	var identity string
	data, err := os.ReadFile(otherMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscanf(string(data), "child:%d:%s", &pid, &identity); err != nil {
		t.Fatalf("descendant did not record itself: %q: %v", data, err)
	}
	for deadline := time.Now().Add(markerBound); ; time.Sleep(5 * time.Millisecond) {
		if r, err := readProcessRecord(pid); err == nil && r.parent == os.Getpid() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d was never reparented to the test", pid)
		}
	}
	root, s, marker, _ := startSupervised(t, "grandchild_hold")
	cleanupGrandchild(t, marker)
	// Recorded, never sent.
	s.signal = func(int, syscall.Signal) error { return nil }
	tree := s.stopAll()
	if _, ok := tree[root.Process.Pid]; !ok {
		t.Fatalf("the tree misses its own root: %v", tree)
	}
	if _, ok := tree[pid]; ok {
		t.Fatalf("the supervisor took %d, an orphan in another open supervisor's session", pid)
	}
	other.close()
	if err := waitProcessGone(pid, identity, 10*time.Second); err != nil {
		t.Fatalf("its own supervisor's close left the orphan: %v", err)
	}
}

func TestASupervisorHoldsOnlyWhatItCanIdentify(t *testing.T) {
	s := &processSupervisor{
		pid: 100, created: "c100",
		sessions: map[int]string{200: "c200"},
		seen:     map[int]processRecord{300: {pid: 300, created: "c300"}},
	}
	records := map[int]processRecord{
		200: {pid: 200, session: 200, created: "c200"},
		400: {pid: 400, session: 400, created: "other"},
	}
	for _, c := range []struct {
		name   string
		record processRecord
		want   bool
	}{
		{"its root", processRecord{pid: 100, session: 100, created: "c100"}, true},
		{"a pid reused for its root", processRecord{pid: 100, session: 1, created: "reused"}, false},
		{"its root's session, the root gone", processRecord{pid: 101, session: 100, created: "x"}, true},
		{"a session it recorded", processRecord{pid: 201, session: 200, created: "x"}, true},
		{"a process its scans saw", processRecord{pid: 300, session: 300, created: "c300"}, true},
		{"a pid reused for one its scans saw", processRecord{pid: 300, session: 300, created: "reused"}, false},
		{"a session it never recorded", processRecord{pid: 401, session: 400, created: "x"}, false},
	} {
		if got := s.holds(c.record, records); got != c.want {
			t.Errorf("%s: holds = %v, want %v", c.name, got, c.want)
		}
	}
	// A session, its root's or one it recorded, whose leader is now another
	// process is not its own.
	records[100] = processRecord{pid: 100, session: 100, created: "reused"}
	records[200] = processRecord{pid: 200, session: 200, created: "reused"}
	for _, session := range []int{100, 200} {
		if s.holds(processRecord{pid: session + 1, session: session, created: "x"}, records) {
			t.Errorf("holds took a member of session %d, whose leader id was reused", session)
		}
	}
}

func TestAFailedSubreaperCallIsReported(t *testing.T) {
	failing := func(int, uintptr, uintptr, uintptr, uintptr) error { return errors.New("refused") }
	if err := subreaper(failing); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("subreaper = %v, want the prctl error", err)
	}
}
