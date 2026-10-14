package executor

import (
	"os"
	"os/exec"
	"testing"
	"time"
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
		_ = cmd.Wait()
		t.Fatalf("process supervision hid immediate exit: %v", err)
	}
	err = cmd.Wait()
	if err == nil || cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("exit = %v, state = %v", err, cmd.ProcessState)
	}
}
