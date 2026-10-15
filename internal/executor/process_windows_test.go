package executor

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
