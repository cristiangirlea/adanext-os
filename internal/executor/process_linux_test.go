package executor

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

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
