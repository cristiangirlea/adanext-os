package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return processRecord{}, err
	}
	return processRecord{pid: pid, parent: parent, group: group, session: session, created: fields[19]}, nil
}

func processCreated(pid int) string {
	record, err := readProcessRecord(pid)
	if err != nil {
		return ""
	}
	return record.created
}

// scanTree lists the engine's session and every descendant of its members by
// parentage, which also finds one that started a session of its own. Linux
// keeps a session id reserved while any member lives, so once the engine has
// exited the id still names only what it left behind; a live process with the
// engine's pid and another creation time means nothing can be told apart.
func scanTree(root int, created string) map[int]processRecord {
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
	if engine, ok := records[root]; ok && engine.created != created {
		return nil
	}
	tree := make(map[int]processRecord)
	var queue []int
	for pid, record := range records {
		if pid == root || record.session == root {
			tree[pid] = record
			queue = append(queue, pid)
		}
	}
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent] {
			if _, seen := tree[pid]; !seen {
				tree[pid] = records[pid]
				queue = append(queue, pid)
			}
		}
	}
	return tree
}

// stopTree stops the engine's tree group by group and scans again until a scan
// finds nothing new: a stopped process forks nothing, and Linux restarts a
// fork that a group signal interrupts, so the tree it returns is complete.
func stopTree(signal func(int, syscall.Signal) error, root int, created string) map[int]processRecord {
	if created == "" {
		return nil
	}
	tree := make(map[int]processRecord)
	for range 32 {
		current := scanTree(root, created)
		grew := false
		for pid, record := range current {
			if _, ok := tree[pid]; !ok {
				tree[pid] = record
				grew = true
			}
		}
		if !grew {
			break
		}
		signalGroups(signal, current, syscall.SIGSTOP)
	}
	return tree
}

// killTree kills each group of the tree stopTree found.
func killTree(signal func(int, syscall.Signal) error, tree map[int]processRecord) {
	signalGroups(signal, tree, syscall.SIGKILL)
}

// signalGroups signals each group once, through a member that is still the
// process scanned, so a group id is never signalled on its number alone.
func signalGroups(signal func(int, syscall.Signal) error, tree map[int]processRecord, sig syscall.Signal) {
	done := make(map[int]bool)
	for pid, record := range tree {
		current, err := readProcessRecord(pid)
		if err != nil || current.created != record.created || done[current.group] {
			continue
		}
		done[current.group] = true
		_ = signal(-current.group, sig)
	}
}

func (s *processSupervisor) scanWithSeen() map[int]processRecord {
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
	if engine, ok := records[s.pid]; ok && engine.created != s.created {
		return nil
	}
	self := os.Getpid()
	cleanDir := ""
	if s.dir != "" {
		if resolved, err := filepath.EvalSymlinks(s.dir); err == nil {
			cleanDir = filepath.Clean(resolved)
		} else {
			cleanDir = filepath.Clean(s.dir)
		}
	}
	tree := make(map[int]processRecord)
	var queue []int
	add := func(pid int, rec processRecord) {
		if _, ok := tree[pid]; !ok {
			tree[pid] = rec
			queue = append(queue, pid)
		}
	}
	for pid, record := range records {
		if pid == s.pid || record.session == s.pid || s.sessions[record.session] {
			add(pid, record)
			continue
		}
		if prev, ok := s.seen[pid]; ok && prev.created == record.created {
			add(pid, record)
			continue
		}
		if cleanDir != "" && pid != self {
			if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
				cwd = filepath.Clean(cwd)
				if cwd == cleanDir || strings.HasPrefix(cwd, cleanDir+"/") {
					add(pid, record)
				}
			}
		}
	}
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent] {
			add(pid, records[pid])
		}
	}
	return tree
}

func (s *processSupervisor) stopAll() map[int]processRecord {
	if s.created == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[int]processRecord{}
	}
	if s.sessions == nil {
		s.sessions = map[int]bool{}
	}
	tree := make(map[int]processRecord)
	for range 32 {
		current := s.scanWithSeen()
		grew := false
		for pid, record := range current {
			s.seen[pid] = record
			if record.session != 0 {
				s.sessions[record.session] = true
			}
			if _, ok := tree[pid]; !ok {
				tree[pid] = record
				grew = true
			}
		}
		if !grew {
			break
		}
		signalGroups(s.signal, current, syscall.SIGSTOP)
	}
	return tree
}

func waitTree(tree map[int]processRecord) {
	if len(tree) == 0 {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for pid, record := range tree {
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err != nil {
				break
			}
			end := strings.LastIndexByte(string(data), ')')
			if end < 0 {
				break
			}
			fields := strings.Fields(string(data[end+1:]))
			if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" || fields[19] != record.created {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
