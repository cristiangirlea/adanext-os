package executor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type processRecord struct {
	pid, parent, group int
	created            string
}

func readProcessRecord(pid int) (processRecord, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processRecord{}, err
	}
	// comm may contain spaces and parentheses; fields after its final ')' start at state.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return processRecord{}, fmt.Errorf("invalid stat for %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return processRecord{}, fmt.Errorf("short stat for %d", pid)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return processRecord{}, err
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return processRecord{}, err
	}
	return processRecord{pid: pid, parent: parent, group: group, created: fields[19]}, nil
}

// The map records group leaders' creation times, not just reusable numeric PIDs.
func descendantGroups(root int) map[int]string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	records := make(map[int]processRecord)
	children := make(map[int][]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		record, err := readProcessRecord(pid)
		if err != nil {
			continue
		}
		records[pid] = record
		children[record.parent] = append(children[record.parent], pid)
	}
	groups := make(map[int]string)
	visited := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent] {
			if visited[pid] {
				continue
			}
			visited[pid] = true
			record := records[pid]
			if record.group > 0 && record.group != root {
				if leader, ok := records[record.group]; ok {
					groups[record.group] = leader.created
				}
			}
			queue = append(queue, pid)
		}
	}
	return groups
}

func killDescendantGroups(groups map[int]string) {
	for group, created := range groups {
		leader, err := readProcessRecord(group)
		if err != nil || leader.created != created {
			continue
		}
		_ = syscall.Kill(-group, syscall.SIGKILL)
	}
}
