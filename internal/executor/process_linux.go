package executor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// becomeSubreaper makes adad the parent of every orphan of its trees, so a
// daemon that leaves its session after its parent exits is still found. adad
// reaps only processes a supervisor tracked, by pid, never with wait(-1), so
// no exit status os/exec waits for is taken.
var becomeSubreaper = sync.OnceValue(func() error { return subreaper(unix.Prctl) })

// subreaper makes the call through prctl, the test seam.
func subreaper(prctl func(option int, arg2, arg3, arg4, arg5 uintptr) error) error {
	if err := prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("become child subreaper: %w", err)
	}
	return nil
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
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return processRecord{}, err
	}
	return processRecord{pid: pid, parent: parent, group: group, session: session, created: fields[19], state: fields[0]}, nil
}

func processCreated(pid int) string {
	record, err := readProcessRecord(pid)
	if err != nil {
		return ""
	}
	return record.created
}

func exited(r processRecord) bool { return r.state == "Z" || r.state == "X" }

// otherSupervisors reports the pids of the other supervisors' roots, and
// whether any other may still own an orphan: one not started yet, or whose
// root still runs.
func (s *processSupervisor) otherSupervisors(records map[int]processRecord) (roots map[int]bool, live bool) {
	roots = map[int]bool{}
	supervisorsMu.Lock()
	defer supervisorsMu.Unlock()
	for other := range supervisors {
		if other == s {
			continue
		}
		if other.pid == 0 {
			live = true
			continue
		}
		roots[other.pid] = true
		if r, ok := records[other.pid]; ok && r.created == other.created && !exited(r) {
			live = true
		}
	}
	return roots, live
}

// scan lists the tree: the root, its session, the sessions its real
// descendants lead, what earlier scans saw, every descendant of those by
// parentage, and, while no other supervisor could own them, the orphans
// reparented to adad outside its own session. Every process adad starts in a
// session of its own is a supervisor's root, so such an orphan came from a
// supervised tree. A process is never taken for sharing a working directory.
func (s *processSupervisor) scan() map[int]processRecord {
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
	self := os.Getpid()
	own := records[self]
	roots, live := s.otherSupervisors(records)
	// A session id stays reserved while any member lives: it names the
	// session we know unless its leader is now another process.
	inSession := func(r processRecord) bool {
		leader, ok := records[r.session]
		switch {
		case r.session == s.pid:
			return !ok || leader.created == s.created
		case s.sessions[r.session] != "":
			return !ok || leader.created == s.sessions[r.session]
		}
		return false
	}
	tree := make(map[int]processRecord)
	var queue []int
	add := func(pid int, rec processRecord) {
		if _, ok := tree[pid]; !ok && pid != self {
			tree[pid] = rec
			queue = append(queue, pid)
		}
	}
	for pid, record := range records {
		prev, seen := s.seen[pid]
		switch {
		case pid == s.pid && record.created == s.created, inSession(record), seen && prev.created == record.created:
			add(pid, record)
		case record.parent == self && record.session != own.session && !roots[pid] && pid != s.pid && !live:
			add(pid, record)
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

// stopAll stops the tree group by group and scans again until a scan finds
// nothing new: a stopped process forks nothing, and Linux restarts a fork that
// a group signal interrupts, so the tree it returns is complete.
func (s *processSupervisor) stopAll() map[int]processRecord {
	if s.created == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tree := make(map[int]processRecord)
	for range 32 {
		current := s.scan()
		grew := false
		for pid, record := range current {
			s.seen[pid] = record
			// Only a session a real descendant leads is followed.
			if record.session == pid {
				s.sessions[pid] = record.created
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

// killTree kills each group of the tree stopAll found.
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

// waitTree waits, bounded, until each process of the tree has exited, and
// reaps one that exited as adad's child: an orphan adad took as subreaper,
// never the root (os/exec waits for it) nor another supervisor's root. After
// the deadline one more pass reaps every member that has exited by then, so
// one stuck process leaks no zombies.
func (s *processSupervisor) waitTree(tree map[int]processRecord) {
	s.waitTreeWithin(tree, 5*time.Second)
}

func (s *processSupervisor) waitTreeWithin(tree map[int]processRecord, bound time.Duration) {
	if len(tree) == 0 {
		return
	}
	deadline := time.Now().Add(bound)
	for pid, record := range tree {
		for time.Now().Before(deadline) && !s.reap(pid, record) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	for pid, record := range tree {
		s.reap(pid, record)
	}
}

// reap reports a member that is gone or has exited, and reaps it, without
// blocking, when it exited as adad's own orphan.
func (s *processSupervisor) reap(pid int, record processRecord) bool {
	current, err := readProcessRecord(pid)
	if err != nil || current.created != record.created {
		return true
	}
	if !exited(current) {
		return false
	}
	if current.parent == os.Getpid() && pid != s.pid && !s.isRoot(pid) {
		var status unix.WaitStatus
		_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
	}
	return true
}

// isRoot reports another supervisor's root, which its own cmd.Wait reaps.
func (s *processSupervisor) isRoot(pid int) bool {
	supervisorsMu.Lock()
	defer supervisorsMu.Unlock()
	for other := range supervisors {
		if other != s && other.pid == pid {
			return true
		}
	}
	return false
}
