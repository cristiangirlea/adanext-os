package executor

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processSupervisor struct {
	mu  sync.Mutex
	job windows.Handle
}

func newProcessSupervisor(*exec.Cmd) (*processSupervisor, error) {
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
	return &processSupervisor{job: job}, nil
}

func (s *processSupervisor) started(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(s.job, process)
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
