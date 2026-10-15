package executor

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processSupervisor struct {
	mu      sync.Mutex
	job     windows.Handle
	threads func(pid uint32) ([]uint32, error) // Test seams: the thread walk and the resume call.
	resume  func(tid uint32) error
}

func newProcessSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	return &processSupervisor{job: job, threads: processThreads, resume: resumeThread}, nil
}

func (s *processSupervisor) started(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(s.job, process); err != nil {
		return fmt.Errorf("assign suspended engine to job: %w", err)
	}
	ids, err := s.threads(uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("snapshot engine threads: %w", err)
	}
	// Resume every thread: an injected one (security software, a debugger)
	// can precede the initial thread in the snapshot, and resuming a running
	// thread is harmless. A thread gone since the snapshot is tolerated; only
	// resuming nothing leaves the engine suspended, so that is the error.
	var failures []string
	resumed := 0
	for _, id := range ids {
		if err := s.resume(id); err != nil {
			failures = append(failures, fmt.Sprintf("thread %d: %v", id, err))
			continue
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("resume engine threads: none of %d resumed: %s", len(ids), strings.Join(failures, "; "))
	}
	return nil
}

// processThreads lists the threads of a process in snapshot order.
func processThreads(pid uint32) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var ids []uint32
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == pid {
			ids = append(ids, entry.ThreadID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return ids, err
	}
	return ids, nil
}

func resumeThread(tid uint32) error {
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)
	if err != nil {
		return fmt.Errorf("open suspended engine thread: %w", err)
	}
	_, err = windows.ResumeThread(thread)
	windows.CloseHandle(thread)
	if err != nil {
		return fmt.Errorf("resume engine thread: %w", err)
	}
	return nil
}

func (s *processSupervisor) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != 0 {
		_ = windows.CloseHandle(s.job)
		s.job = 0
	}
}

func (s *processSupervisor) kill(*exec.Cmd) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return nil
	}
	err := windows.CloseHandle(s.job)
	s.job = 0
	return err
}
