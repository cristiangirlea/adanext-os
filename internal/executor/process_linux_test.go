package executor

import (
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
