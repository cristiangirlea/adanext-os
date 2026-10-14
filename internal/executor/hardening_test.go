package executor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestPreparingDeadlineFailsEnginesThatDoNotCompleteTheHandshake(t *testing.T) {
	for _, script := range []string{"no_hello", "no_start_answer"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.cfg.PreparingDeadline = 100 * time.Millisecond
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "ended_failed")
			task, err := s.Task(context.Background(), "u", "task")
			if err != nil || task.State != store.Failed {
				t.Fatalf("task = %+v: %v", task, err)
			}
			errorEventContains(t, s, "preparing deadline")
		})
	}
}

func TestSlowExitAfterSuccessfulResultRequiresRecovery(t *testing.T) {
	e, s, _, _ := testExecutor(t, "big_block")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "recovery_required")
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.RecoveryRequired {
		t.Fatalf("task = %+v: %v", task, err)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	var result, notice bool
	for _, event := range events {
		if event.Kind == "result" && strings.Contains(string(event.Payload), `"status":"ok"`) {
			result = true
		}
		if event.Kind == "recovery" && strings.Contains(string(event.Payload), "killed after its result") {
			notice = true
		}
	}
	if !result || !notice {
		t.Fatalf("result=%v notice=%v events=%+v", result, notice, events)
	}
}

func TestUnknownEventKindFailsAsAProtocolViolation(t *testing.T) {
	e, s, _, _ := testExecutor(t, "unknown_event_kind")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, "event kind")
}

func TestNonfatalEngineErrorIsStoredAsAnEngineNotice(t *testing.T) {
	e, s, _, _ := testExecutor(t, "nonfatal_error")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "engine_notice" && strings.Contains(string(event.Payload), "bad_request") {
			return
		}
	}
	t.Fatalf("missing engine notice: %+v", events)
}

func TestEngineErrorEventKeepsItsProtocolKind(t *testing.T) {
	e, s, _, _ := testExecutor(t, "engine_error_event")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "error" && strings.Contains(string(event.Payload), "backend warning") {
			return
		}
	}
	t.Fatalf("missing engine error event: %+v", events)
}

func TestStreamingDescendantCannotExtendTheExitDeadline(t *testing.T) {
	e, s, _, marker := testExecutor(t, "stream_after_exit")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 100 * time.Millisecond
	started := time.Now()
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	if elapsed := time.Since(started); elapsed < e.cfg.CancelGrace || elapsed > 700*time.Millisecond {
		t.Fatalf("attempt took %s", elapsed)
	}
}

func TestReadStartingBeforeIdleCheckKeepsStdoutOpenForThatCheck(t *testing.T) {
	e, s, _, marker := testExecutor(t, "exit_with_child_stdout")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 200 * time.Millisecond
	first := make(chan struct{})
	var checks atomic.Int32
	e.beforeIdleCheck = func(progress *atomic.Int64, reading *atomic.Bool) {
		if checks.Add(1) == 1 {
			progress.Add(1)
			reading.Store(true)
			close(first)
		}
	}
	e.LaunchAttempt("attempt")
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("idle check did not run")
	}
	time.Sleep(50 * time.Millisecond)
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	if a.State == "ended_failed" {
		t.Fatal("stdout closed during the check after a read started")
	}
	waitAttempt(t, s, "ended_failed")
}

func TestForceKillStopsTheGrandchildWithTheEngine(t *testing.T) {
	e, s, _, marker := testExecutor(t, "grandchild_after_result")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 100 * time.Millisecond
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "recovery_required")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	var identity string
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &identity); err == nil {
			break
		}
	}
	if pid == 0 {
		t.Fatal("grandchild did not record identity")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := processIdentity(pid)
		if err != nil || current != identity {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild %d with creation time %s survived engine kill", pid, identity)
}

func TestCancelAfterSuccessfulResultRequiresRecovery(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%v", stop), func(t *testing.T) {
			e, s, _, _ := testExecutor(t, "big_block")
			e.cfg.CancelGrace = 500 * time.Millisecond
			e.LaunchAttempt("attempt")
			deadline := time.Now().Add(3 * time.Second)
			found := false
			for time.Now().Before(deadline) {
				events, _ := s.Events(context.Background(), "u", "attempt", 0)
				for _, event := range events {
					if event.Kind == "result" {
						found = true
					}
				}
				if found {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !found {
				t.Fatal("result not stored")
			}
			if stop {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := e.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				e.CancelTask("task")
			}
			waitAttempt(t, s, "recovery_required")
			task, err := s.Task(context.Background(), "u", "task")
			if err != nil || task.State != store.RecoveryRequired {
				t.Fatalf("task=%+v: %v", task, err)
			}
			events, err := s.Events(context.Background(), "u", "attempt", 0)
			found = false
			for _, event := range events {
				if event.Kind == "result" && strings.Contains(string(event.Payload), `"status":"ok"`) {
					found = true
				}
			}
			if err != nil || !found {
				t.Fatalf("result lost: %+v: %v", events, err)
			}
		})
	}
}

func TestPreparingDeadlineAsAcceptedArrives(t *testing.T) {
	e, s, _, _ := testExecutor(t, "ignore_cancel")
	e.beforeAccepted = func(timer *time.Timer, expired *atomic.Bool) {
		timer.Reset(0)
		for !expired.Load() {
			time.Sleep(time.Millisecond)
		}
	}
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, "preparing deadline exceeded")
}

func TestImmediatelyExitingEngineReportsExitCause(t *testing.T) {
	e, s, _, _ := testExecutor(t, "exit_immediately")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, "exit status 23")
	events, _ := s.Events(context.Background(), "u", "attempt", 0)
	for _, event := range events {
		if strings.Contains(string(event.Payload), "process supervision") {
			t.Fatalf("wrong cause: %s", event.Payload)
		}
	}
}

func TestForceKillStopsNestedBackendAndGrandchild(t *testing.T) {
	e, s, _, marker := testExecutor(t, "nested_tree")
	cleanupGrandchild(t, marker)
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "recovery_required")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		var pid int
		var identity string
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &identity); err != nil {
			continue
		}
		count++
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			current, err := processIdentity(pid)
			if err != nil || current != identity {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if current, err := processIdentity(pid); err == nil && current == identity {
			t.Errorf("descendant %d with creation time %s survived forced kill", pid, identity)
		}
	}
	if count != 2 {
		t.Fatalf("got %d descendants, want backend and grandchild: %s", count, data)
	}
}

func TestFatalNoticeWithOKResultRecoversOnlyAfterKill(t *testing.T) {
	for _, script := range []string{"fatal_ok", "fatal_ok_block", "fatal_ok_bad_tail_block"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			want := "recovery_required"
			if script == "fatal_ok" {
				want = "ended_failed"
			}
			waitAttempt(t, s, want)
			events, err := s.Events(context.Background(), "u", "attempt", 0)
			var notice, result bool
			for _, event := range events {
				if event.Kind == "engine_notice" {
					notice = true
				}
				if event.Kind == "result" && strings.Contains(string(event.Payload), `"status":"ok"`) {
					result = true
				}
			}
			if err != nil || !notice || !result {
				t.Fatalf("notice=%v result=%v: %v", notice, result, err)
			}
		})
	}
}

func TestResultRecoveryStorageFailureFinalizesAttempt(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	ctx := context.Background()
	task, err := s.Task(ctx, "u", "task")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "u", "task", store.Preparing, store.Running, task.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "u", "recovery-fault", "recovery-fault", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='running' WHERE id='attempt'")
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `CREATE TRIGGER recovery_fault BEFORE UPDATE OF state ON attempts WHEN NEW.state='recovery_required' BEGIN SELECT RAISE(ABORT, 'recovery unavailable'); END`)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	e.recoverAfterResult(a)
	a, err = s.AttemptByID(ctx, "attempt")
	if err != nil || a.State != "ended_failed" || a.Ended == "" {
		t.Fatalf("attempt=%+v: %v", a, err)
	}
	task, err = s.Task(ctx, "u", "task")
	if err != nil || task.State != store.Failed {
		t.Fatalf("task=%+v: %v", task, err)
	}
	errorEventContains(t, s, "recovery unavailable")
}
