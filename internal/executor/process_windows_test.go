package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

func TestWindowsSuspendedEngineCannotExitBeforeAssignment(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
	cmd.Env = append(os.Environ(), "ADA_FAKE_ENGINE=exit_immediately")
	s, err := newProcessSupervisor(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	// Give an unsuspended process ample time to exit before assignment.
	time.Sleep(300 * time.Millisecond)
	if err := s.started(cmd); err != nil {
		// The process may still be suspended: kill it before waiting.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("process supervision hid immediate exit: %v", err)
	}
	err = cmd.Wait()
	if err == nil || cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("exit = %v, state = %v", err, cmd.ProcessState)
	}
}

func TestWindowsStartupRefusesAnEmptyThreadSnapshot(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
	cmd.Env = append(os.Environ(), "ADA_FAKE_ENGINE=exit_immediately")
	s, err := newProcessSupervisor(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.threads = func(uint32) ([]uint32, error) { return nil, nil }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err = s.started(cmd)
	// The engine stays suspended either way: end it before judging the error.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err == nil || !strings.Contains(err.Error(), "none of 0 resumed") {
		t.Fatalf("started = %v, want the empty snapshot refused", err)
	}
}

func TestWindowsStartupResumesTheInitialThreadBehindAnInjectedThread(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "injected thread resumes"
		if stale {
			name = "injected thread is stale"
		}
		t.Run(name, func(t *testing.T) {
			// A live thread of this process stands in for one injected into the
			// engine ahead of its initial thread (security software, a debugger).
			injected := make(chan uint32, 1)
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				runtime.LockOSThread()
				injected <- windows.GetCurrentThreadId()
				<-stop
			}()
			tid := <-injected
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
			cmd.Env = append(os.Environ(), "ADA_FAKE_ENGINE=exit_immediately")
			s, err := newProcessSupervisor(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			threads, resume := s.threads, s.resume
			s.threads = func(pid uint32) ([]uint32, error) {
				ids, err := threads(pid)
				return append([]uint32{tid}, ids...), err
			}
			if stale {
				s.resume = func(id uint32) error {
					if id == tid {
						return errors.New("the thread exited after the snapshot")
					}
					return resume(id)
				}
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if err := s.started(cmd); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("started: %v", err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err := <-waited:
				if cmd.ProcessState.ExitCode() != 23 {
					t.Fatalf("exit = %v, state = %v", err, cmd.ProcessState)
				}
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-waited
				t.Fatal("the initial thread stayed suspended behind the injected thread")
			}
		})
	}
}

// newBackendSupervisor mirrors Harness on Windows: a nested kill-on-close Job
// Object, without breakaway.
func newBackendSupervisor(cmd *exec.Cmd) (*processSupervisor, error) {
	return newProcessSupervisor(cmd)
}

func TestWindowsCloseTerminatesAndWaitsForJobProcesses(t *testing.T) {
	marker := t.TempDir() + `\marker`
	held := t.TempDir() + `\held`
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
	cmd.Dir = held
	cmd.Env = append(os.Environ(), "ADA_FAKE_ENGINE=detached_child", "ADA_FAKE_MARKER="+marker)
	s, err := newProcessSupervisor(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	cleanupGrandchild(t, marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	// The parent exits after starting its detached child.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("parent wait: %v", err)
	}
	waitGrandchildMarker(t, marker)
	// A duplicate handle keeps the job object alive across CloseHandle, so only
	// an explicit TerminateJobObject and bounded wait ends the detached child before close returns.
	var dup windows.Handle
	self := windows.CurrentProcess()
	if err := windows.DuplicateHandle(self, s.job, self, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(dup)

	var pid int
	var identity string
	for _, line := range markerLines(t, marker) {
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &identity); err == nil && pid > 0 {
			break
		}
	}
	if pid == 0 {
		t.Fatal("missing detached child pid")
	}
	s.close()
	if err := waitProcessGone(pid, identity, 0); err != nil {
		t.Fatalf("detached child still running after close returned: %v", err)
	}
	if err := os.RemoveAll(held); err != nil {
		t.Fatalf("remove held directory after close: %v", err)
	}
}
