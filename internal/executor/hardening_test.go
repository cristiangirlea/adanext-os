package executor

import (
	"context"
	"fmt"
	"os"
	"slices"
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
	e.cfg.CancelGrace = naturalExitGrace
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
	e.cfg.CancelGrace = naturalExitGrace
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
	// Two process starts precede the exit; a descendant that could extend the
	// deadline would instead keep the attempt open for as long as it streams.
	if elapsed := time.Since(started); elapsed < e.cfg.CancelGrace || elapsed > 3*e.cfg.CancelGrace+time.Second {
		t.Fatalf("attempt took %s", elapsed)
	}
}

func TestReadsBeforeTheEngineExitsDoNotCountAgainstADescendant(t *testing.T) {
	var r readState
	r.begin()
	r.end(5)
	r.begin()
	time.Sleep(50 * time.Millisecond)
	exit := time.Now()
	r.exited(exit)
	n, blocked := r.afterExit()
	if n != 0 || blocked < 0 || blocked >= 50*time.Millisecond {
		t.Fatalf("bytes=%d blocked=%s after an ended read and a read begun before exit", n, blocked)
	}
	r.end(7)
	if n, blocked := r.afterExit(); n != 7 || blocked > time.Since(exit) {
		t.Fatalf("bytes=%d blocked=%s for a read that straddled exit", n, blocked)
	}
}

func TestReadsAfterTheEngineExitsAccumulateBlockedTimeAndBytes(t *testing.T) {
	var r readState
	r.exited(time.Now())
	r.begin()
	time.Sleep(30 * time.Millisecond)
	r.end(3)
	r.begin()
	time.Sleep(30 * time.Millisecond)
	n, blocked := r.afterExit()
	if n != 3 || blocked < 60*time.Millisecond {
		t.Fatalf("bytes=%d blocked=%s with one ended and one in-progress read", n, blocked)
	}
	r.end(4)
	if n, blocked := r.afterExit(); n != 7 || blocked < 60*time.Millisecond {
		t.Fatalf("bytes=%d blocked=%s after both reads ended", n, blocked)
	}
}

func TestSlowlyWritingDescendantCannotHoldStopPastItsBudget(t *testing.T) {
	e, s, _, marker := testExecutor(t, "slow_stream_after_exit")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 200 * time.Millisecond
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_INTERVAL=100ms")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "running")
	waitGrandchildMarker(t, marker)
	ctx, cancel := context.WithTimeout(context.Background(), e.StopBudget())
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
}

func TestPreparingDeadlineEndsAnEngineThatNeverReadsStdin(t *testing.T) {
	for _, script := range []string{"hello_no_read", "hello_partial_read"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, marker := testExecutor(t, script)
			e.cfg.PreparingDeadline = 100 * time.Millisecond
			ctx := context.Background()
			// A prompt larger than any pipe buffer: the start line cannot be
			// written unless the engine reads all of it.
			_, err := s.Do(ctx, "u", "big-objective", "big-objective", nil, func(tx *store.Tx) ([]byte, error) {
				_, err := tx.ExecContext(ctx, "UPDATE tasks SET objective=? WHERE id='task'", strings.Repeat("x", 128<<10))
				return []byte("ok"), err
			})
			if err != nil {
				t.Fatal(err)
			}
			e.LaunchAttempt("attempt")
			a := waitAttempt(t, s, "ended_failed")
			// The deadline runs from the engine's start, which precedes its
			// recording itself. On a loaded machine the kill can come before
			// the engine records itself at all; the error events below still
			// show the deadline and the kill.
			if _, err := os.Stat(marker); err == nil {
				if span := markerToEnd(t, marker, a); span > e.cfg.PreparingDeadline+3*e.cfg.CancelGrace+time.Second {
					t.Fatalf("attempt ended %s after the engine started", span)
				}
			}
			errorEventContains(t, s, "preparing deadline exceeded")
			errorEventContains(t, s, "engine was killed after the grace period")
		})
	}
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
				ctx, cancel := context.WithTimeout(context.Background(), e.StopBudget())
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
			want := "recovery_required"
			if script == "fatal_ok" {
				want = "ended_failed"
				e.cfg.CancelGrace = naturalExitGrace
			}
			e.LaunchAttempt("attempt")
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

func TestStopEndsADescendantFeedingValidEventsFasterThanStoragePersistsThem(t *testing.T) {
	t.Parallel()
	e, s, _, marker := testExecutor(t, "valid_stream_after_exit")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = naturalExitGrace
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_INTERVAL=100ms")
	e.afterAppend = func() { time.Sleep(150 * time.Millisecond) }
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "running")
	waitGrandchildMarker(t, marker)
	// The engine exits right after the marker; wait until the descendant's
	// events are being ingested so the stop arrives after the exit.
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, _ := s.Events(context.Background(), "u", "attempt", 0)
		if len(events) >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant events not ingested: %+v", events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.StopBudget())
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	errorEventContains(t, s, "after the engine exited")
}

func TestStopAfterTheEngineExitsCutsOffTheBufferedEventBacklog(t *testing.T) {
	t.Parallel()
	e, s, _, marker := testExecutor(t, "burst_before_exit")
	e.cfg.CancelGrace = naturalExitGrace
	// 110 buffered events at 300 ms each outlast StopBudget's 30 s.
	e.afterAppend = func() { time.Sleep(300 * time.Millisecond) }
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "running")
	// The engine marks its exit once the whole burst is in the pipe; the
	// reader's buffer then holds more events than StopBudget can persist.
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Contains(markerLines(t, marker), "exiting") {
		if time.Now().After(deadline) {
			t.Fatal("engine did not exit behind its burst")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.StopBudget())
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	errorEventContains(t, s, "a cancel or stop arrived after the engine exited")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	persisted := 0
	for _, event := range events {
		if event.Kind == "text" {
			persisted++
		}
	}
	if persisted >= 111 {
		t.Fatalf("all %d buffered events were persisted before the stop took effect", persisted)
	}
}

func TestPartialLineAfterASuccessfulResultFailsTheAttempt(t *testing.T) {
	t.Parallel()
	e, s, _, marker := testExecutor(t, "ok_partial_tail_hold")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = naturalExitGrace
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.Failed {
		t.Fatalf("task = %+v: %v", task, err)
	}
	errorEventContains(t, s, "truncated protocol line")
}
