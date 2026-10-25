package executor

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobObjectBasicAccountingInformation = 1
	jobObjectBasicProcessIDList         = 3
	jobExitTimeout                      = 5 * time.Second
)

var procQueryInformationJobObject = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryInformationJobObject")

type jobBasicAccountingInfo struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type jobBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [256]uintptr
}

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
		_ = terminateAndCloseJob(s.job)
		s.job = 0
	}
}

func (s *processSupervisor) kill(*exec.Cmd) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return nil
	}
	err := terminateAndCloseJob(s.job)
	s.job = 0
	return err
}

// terminateAndCloseJob terminates the Job Object, waits boundedly for its
// processes to exit, and closes the handle.
func terminateAndCloseJob(job windows.Handle) error {
	handles := map[uint32]windows.Handle{}
	collectJobProcesses(job, handles)
	_ = windows.TerminateJobObject(job, 1)
	collectJobProcesses(job, handles)
	deadline := time.Now().Add(jobExitTimeout)
	for _, h := range handles {
		wait := max(time.Until(deadline), 0)
		_, _ = windows.WaitForSingleObject(h, uint32(wait/time.Millisecond))
		_ = windows.CloseHandle(h)
	}
	for time.Now().Before(deadline) {
		var info jobBasicAccountingInfo
		r1, _, _ := procQueryInformationJobObject.Call(
			uintptr(job),
			jobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
			0,
		)
		if r1 == 0 || info.ActiveProcesses == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return windows.CloseHandle(job)
}

func collectJobProcesses(job windows.Handle, handles map[uint32]windows.Handle) {
	var list jobBasicProcessIDList
	r1, _, _ := procQueryInformationJobObject.Call(
		uintptr(job),
		jobObjectBasicProcessIDList,
		uintptr(unsafe.Pointer(&list)),
		unsafe.Sizeof(list),
		0,
	)
	if r1 == 0 {
		return
	}
	n := min(int(list.NumberOfProcessIdsInList), len(list.ProcessIdList))
	for i := range n {
		pid := uint32(list.ProcessIdList[i])
		if pid == 0 {
			continue
		}
		if _, ok := handles[pid]; ok {
			continue
		}
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid); err == nil {
			handles[pid] = h
		}
	}
}
