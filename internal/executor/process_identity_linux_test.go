package executor

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func processIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("missing process stat name terminator")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("short process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", fmt.Errorf("process has exited")
	}
	return fields[19], nil // field 22, starttime
}

// waitProcessGone waits until the process with this pid and identity has
// exited. A pid that is gone, a zombie, or another process has exited.
func waitProcessGone(pid int, identity string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if current, err := processIdentity(pid); err != nil || current != identity {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d still running after %s", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
