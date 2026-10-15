package executor

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// queueAttempt records a second start the way the API does: the task preparing
// with a pending attempt of this coordinator generation.
func queueAttempt(t *testing.T, e *Executor, s *store.Store, user, task, attempt string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTask(ctx, user, task, "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, user, task, store.Draft, store.Ready, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, user, task, store.Ready, store.Preparing, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAttempt(ctx, store.Attempt{ID: attempt, Task: task, Actor: user, Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: e.cfg.Generation, EngineSHA256: e.cfg.EngineSHA256, State: "pending"}); err != nil {
		t.Fatal(err)
	}
}

func waitAttemptID(t *testing.T, s *store.Store, id string, want ...string) store.Attempt {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		a, err := s.AttemptByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range want {
			if a.State == state {
				return a
			}
		}
		select {
		case <-deadline:
			t.Fatalf("attempt %s state = %s, want one of %v", id, a.State, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func startOrder(t *testing.T, marker string) []string {
	t.Helper()
	var order []string
	for _, line := range markerLines(t, marker) {
		if !strings.HasPrefix(line, "start:") {
			continue
		}
		start := strings.Index(line, `"attempt_id":"`) + len(`"attempt_id":"`)
		order = append(order, line[start:start+strings.Index(line[start:], `"`)])
	}
	return order
}

func TestASecondTaskStartedAtOnceLaunchesOnlyAfterTheFirstEnds(t *testing.T) {
	e, s, dir, marker := testExecutor(t, "block")
	queueAttempt(t, e, s, "u", "task2", "attempt2")
	e.LaunchAttempt("attempt")
	e.LaunchAttempt("attempt2")
	waitAttemptID(t, s, "attempt", "running")
	time.Sleep(50 * time.Millisecond)
	if a, err := s.AttemptByID(context.Background(), "attempt2"); err != nil || a.State != "pending" {
		t.Fatalf("queued attempt = %+v: %v", a, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "attempts", "attempt2")); !os.IsNotExist(err) {
		t.Fatalf("queued attempt has a scratch directory: %v", err)
	}
	if position, err := s.QueuePosition(context.Background(), "task2"); err != nil || position != 1 {
		t.Fatalf("queue position = %d: %v", position, err)
	}
	e.CancelTask("task")
	waitAttemptID(t, s, "attempt", "canceled")
	waitAttemptID(t, s, "attempt2", "running")
	e.CancelTask("task2")
	waitAttemptID(t, s, "attempt2", "canceled")
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt,attempt2" {
		t.Fatalf("start order = %v", order)
	}
}

func TestQueuedAttemptsLaunchInOrderOfCreationAcrossUsers(t *testing.T) {
	e, s, _, marker := testExecutor(t, "block")
	ctx := context.Background()
	if err := s.CreateUser(ctx, "v", "other"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "v", "member"); err != nil {
		t.Fatal(err)
	}
	queueAttempt(t, e, s, "v", "task-b", "b")
	queueAttempt(t, e, s, "u", "task-c", "c")
	// Launch requests arrive in any order; creation order decides.
	e.LaunchAttempt("c")
	e.LaunchAttempt("b")
	e.LaunchAttempt("attempt")
	for _, step := range []struct{ attempt, task string }{{"attempt", "task"}, {"b", "task-b"}, {"c", "task-c"}} {
		waitAttemptID(t, s, step.attempt, "running")
		e.CancelTask(step.task)
		waitAttemptID(t, s, step.attempt, "canceled")
	}
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt,b,c" {
		t.Fatalf("start order = %v", order)
	}
}

func TestACancelRacingTheLaunchEndsCanceledBeforeLaunchOrThroughTheRunningPathNeverBoth(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "block")
	ctx := context.Background()
	e.LaunchAttempt("attempt")
	waitAttemptID(t, s, "attempt", "running")
	e.CancelTask("task")
	waitAttemptID(t, s, "attempt", "canceled")
	withdrawn, launched := 0, 0
	for i := 0; i < 12; i++ {
		task, attempt := fmt.Sprintf("race-task-%d", i), fmt.Sprintf("race-%d", i)
		queueAttempt(t, e, s, "u", task, attempt)
		var wg sync.WaitGroup
		wg.Add(2)
		var cancelErr error
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
			e.LaunchAttempt(attempt)
		}()
		go func() {
			defer wg.Done()
			// The claim needs a queue read and a goroutine before its write;
			// a wider jitter here lets either side win.
			time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
			// What the API's cancel does for a preparing task: withdraw it in
			// the store, and when the launch claim won, tell the executor.
			_, cancelErr = s.Do(ctx, "u", "cancel-"+attempt, "cancel", nil, func(tx *store.Tx) ([]byte, error) {
				_, err := tx.CancelQueued(ctx, task, 2, "withdrawn by u")
				return []byte("ok"), err
			})
			if errors.Is(cancelErr, store.ErrAttemptLaunched) {
				e.CancelTask(task)
			}
		}()
		wg.Wait()
		events, err := s.Events(ctx, "u", attempt, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cancelErr == nil {
			withdrawn++
			// Withdrawn before launch: nothing ran, the executor let it go.
			deadline := time.Now().Add(5 * time.Second)
			for {
				e.mu.Lock()
				_, running := e.active[attempt]
				e.mu.Unlock()
				if !running {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("iteration %d: withdrawn attempt still active", i)
				}
				time.Sleep(5 * time.Millisecond)
			}
			a, err := s.AttemptByID(ctx, attempt)
			if err != nil || a.State != "canceled" {
				t.Fatalf("iteration %d: withdrawn attempt = %+v: %v", i, a, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "attempts", attempt)); !os.IsNotExist(err) {
				t.Fatalf("iteration %d: withdrawn attempt launched: %v", i, err)
			}
			events, err = s.Events(ctx, "u", attempt, 0)
			if err != nil || len(events) != 1 || events[0].Kind != "cancel" {
				t.Fatalf("iteration %d: withdrawn attempt events = %+v: %v", i, events, err)
			}
		} else if errors.Is(cancelErr, store.ErrAttemptLaunched) {
			launched++
			// The claim won: the attempt ends through the running cancel path,
			// and no withdrawal is recorded.
			a := waitAttemptID(t, s, attempt, "canceled", "ended_failed", "recovery_required")
			if _, err := os.Stat(filepath.Join(dir, "attempts", attempt)); err != nil {
				t.Fatalf("iteration %d: launched attempt has no scratch directory: %v", i, err)
			}
			events, err = s.Events(ctx, "u", attempt, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "cancel" {
					t.Fatalf("iteration %d: launched attempt (%s) also withdrawn: %+v", i, a.State, events)
				}
			}
		} else {
			t.Fatalf("iteration %d: cancel = %v", i, cancelErr)
		}
		task2, err := s.Task(ctx, "u", task)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := s.AttemptByID(ctx, attempt)
		want := map[string]store.State{"canceled": store.Canceled, "ended_failed": store.Failed, "recovery_required": store.RecoveryRequired}[a.State]
		if task2.State != want {
			t.Fatalf("iteration %d: task %s with attempt %s", i, task2.State, a.State)
		}
	}
	t.Logf("withdrawn before launch: %d, through the running path: %d", withdrawn, launched)
}

func TestTheOwnersStopCancelsRunningAndQueuedAttemptsWithTheReasonVisibleToTheirActors(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "block")
	ctx := context.Background()
	if err := s.CreateUser(ctx, "v", "other"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "v", "member"); err != nil {
		t.Fatal(err)
	}
	queueAttempt(t, e, s, "v", "task2", "attempt2")
	e.LaunchAttempt("attempt")
	e.LaunchAttempt("attempt2")
	waitAttemptID(t, s, "attempt", "running")
	const reason = "coordinator stopped by the owner"
	// The stop endpoint records the reason in one transaction, then stops.
	_, err := s.Do(ctx, "u", "stop", "stop", nil, func(tx *store.Tx) ([]byte, error) {
		running, queued, err := tx.StopWork(ctx, e.cfg.Generation, reason)
		if err != nil {
			return nil, err
		}
		if running != 1 || queued != 1 {
			t.Errorf("stop work = %d running, %d queued", running, queued)
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.StopWork()
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := e.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ attempt, task, actor string }{{"attempt", "task", "u"}, {"attempt2", "task2", "v"}} {
		a, err := s.AttemptByID(ctx, step.attempt)
		if err != nil || a.State != "canceled" {
			t.Fatalf("attempt %s = %+v: %v", step.attempt, a, err)
		}
		task, err := s.Task(ctx, step.actor, step.task)
		if err != nil || task.State != store.Canceled {
			t.Fatalf("task %s = %+v: %v", step.task, task, err)
		}
		events, err := s.Events(ctx, step.actor, step.attempt, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Kind == "cancel" && strings.Contains(string(event.Payload), `"reason":"`+reason+`"`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("attempt %s events as %s = %+v", step.attempt, step.actor, events)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "attempts", "attempt2")); !os.IsNotExist(err) {
		t.Fatalf("queued attempt launched during the stop: %v", err)
	}
}
