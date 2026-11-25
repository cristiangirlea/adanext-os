package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// execSQL runs trusted SQL against the test store in its own transaction.
func execSQL(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	key := fmt.Sprintf("sql-%d", time.Now().UnixNano())
	_, err := s.Do(context.Background(), "u", key, key, nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// decide records user's decision on the attempt at at, as the API does.
func decide(t *testing.T, s *store.Store, user, attempt, decision, key string, at time.Time) {
	t.Helper()
	_, err := s.Do(context.Background(), user, key, key, nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.Decide(context.Background(), attempt, decision, key, at)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// readonlyDefault makes the fixture's project need a decision for its auto attempt.
func readonlyDefault(t *testing.T, s *store.Store) {
	t.Helper()
	execSQL(t, s, "UPDATE projects SET default_mode='readonly' WHERE id='p'")
}

// staysUnlaunched nudges the scheduler and checks the attempt is still pending with no engine
// started for it.
func staysUnlaunched(t *testing.T, e *Executor, s *store.Store, marker, attempt string) {
	t.Helper()
	e.LaunchAttempt(attempt)
	time.Sleep(150 * time.Millisecond)
	if a, err := s.AttemptByID(context.Background(), attempt); err != nil || a.State != "pending" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	if _, err := os.Stat(marker); err == nil && strings.Contains(strings.Join(startOrder(t, marker), ","), attempt) {
		t.Fatalf("the engine started for %s", attempt)
	}
}

func TestAnAttemptNeedingADecisionNeverLaunchesUntilItsActorAllowsItThenLaunchesOnce(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	readonlyDefault(t, s)
	staysUnlaunched(t, e, s, marker, "attempt")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the engine started: %v", err)
	}
	decide(t, s, "u", "attempt", "allow", "allow", time.Now())
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	e.LaunchAttempt("attempt")
	time.Sleep(50 * time.Millisecond)
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt" {
		t.Fatalf("starts = %v", order)
	}
}

func TestAnAttemptNeedingNoDecisionLaunchesThroughTheSameClaim(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	// The fake engine runs auto alone: a default of auto admits it without a decision.
	execSQL(t, s, "UPDATE projects SET default_mode='auto' WHERE id='p'")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt" {
		t.Fatalf("starts = %v", order)
	}
}

func TestAChangedBindingAfterAnAllowLeavesTheAttemptUnlaunchedUntilAFreshDecision(t *testing.T) {
	for name, change := range map[string]string{
		"task revision":  "UPDATE tasks SET revision=revision+1 WHERE id='task'",
		"snapshot":       "UPDATE tasks SET snapshot='other-snapshot' WHERE id='task'",
		"backend":        "UPDATE attempts SET backend='claude' WHERE id='attempt'",
		"mode":           "UPDATE attempts SET mode='yolo' WHERE id='attempt'",
		"workspace":      "UPDATE attempts SET executor='elsewhere' WHERE id='attempt'",
		"policy version": "UPDATE permission_decisions SET policy_version=policy_version-1",
	} {
		t.Run(name, func(t *testing.T) {
			e, s, _, marker := testExecutor(t, "happy")
			e.cfg.CancelGrace = naturalExitGrace
			readonlyDefault(t, s)
			if name == "snapshot" {
				otherSnapshot(t, s)
			}
			decide(t, s, "u", "attempt", "allow", "first", time.Now())
			execSQL(t, s, change)
			staysUnlaunched(t, e, s, marker, "attempt")
			decide(t, s, "u", "attempt", "allow", "fresh", time.Now())
			e.LaunchAttempt("attempt")
			// The snapshot names no source of this project, so its preparation fails: the
			// attempt still left pending through the claim.
			waitAttemptID(t, s, "attempt", "ended_ok", "ended_failed")
		})
	}
}

// otherSnapshot records a snapshot of another imported project, for a task to be rebound to.
func otherSnapshot(t *testing.T, s *store.Store) {
	t.Helper()
	hash := strings.Repeat("a", 64)
	for _, q := range []string{
		"INSERT INTO projects(id,name,source_path,created) VALUES('o','o','/never-use','now')",
		"INSERT INTO project_sources(project,canonical_path,common_dir,fingerprint,imported) VALUES('o','/o','/o/.git','f','now')",
		"INSERT INTO artifacts(hash,size,origin,created) VALUES('" + hash + "',0,'test','now')",
		"INSERT INTO source_snapshots(id,project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created) VALUES('other-snapshot','o','c','f','" + hash + "','" + hash + "','u','now')",
	} {
		execSQL(t, s, q)
	}
}

func TestARestartSendsAnAllowedUnlaunchedAttemptToRecoveryAndTheNewAttemptNeedsItsOwnDecision(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	readonlyDefault(t, s)
	decide(t, s, "u", "attempt", "allow", "allow", time.Now())
	generation, recovered, err := s.BumpAndRecover(context.Background())
	if err != nil || len(recovered) != 1 || recovered[0] != "attempt" {
		t.Fatalf("recovered %v: %v", recovered, err)
	}
	if a, err := s.AttemptByID(context.Background(), "attempt"); err != nil || a.State != "recovery_required" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	e.cfg.Generation = generation
	queueAttempt(t, e, s, "u", "task2", "attempt2")
	staysUnlaunched(t, e, s, marker, "attempt2")
	decide(t, s, "u", "attempt2", "allow", "allow-2", time.Now())
	e.LaunchAttempt("attempt2")
	waitAttemptID(t, s, "attempt2", "ended_ok")
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt2" {
		t.Fatalf("starts = %v", order)
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func TestAnExpiredAllowDoesNotLaunchOnAFakeClock(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	readonlyDefault(t, s)
	decided := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: decided.Add(store.DecisionLifetime)}
	e.cfg.Clock = clock.Now
	decide(t, s, "u", "attempt", "allow", "allow", decided)
	staysUnlaunched(t, e, s, marker, "attempt")
	clock.set(decided.Add(store.DecisionLifetime - time.Second))
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
}

func TestAnAttemptPassedOverForADecisionDoesNotHoldUpTheNextInTheQueue(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	execSQL(t, s, "UPDATE projects SET default_mode='auto' WHERE id='p'")
	execSQL(t, s, "UPDATE attempts SET mode='yolo' WHERE id='attempt'")
	queueAttempt(t, e, s, "u", "task2", "attempt2")
	e.LaunchAttempt("attempt2")
	waitAttemptID(t, s, "attempt2", "ended_ok")
	if a, err := s.AttemptByID(context.Background(), "attempt"); err != nil || a.State != "pending" {
		t.Fatalf("the attempt waiting for a decision = %+v: %v", a, err)
	}
	if position, err := s.QueuePosition(context.Background(), "task"); err != nil || position != 1 {
		t.Fatalf("its place = %d: %v", position, err)
	}
	if order := startOrder(t, marker); strings.Join(order, ",") != "attempt2" {
		t.Fatalf("starts = %v", order)
	}
}

// changeBeforeClaim runs change once, after the queue chose the attempt and before its claim.
func changeBeforeClaim(t *testing.T, e *Executor, change func()) {
	t.Helper()
	var once sync.Once
	e.beforeClaim = func(string) { once.Do(change) }
}

func TestTheEngineReceivesTheScopeTheClaimJudgedNotTheQueuesCopy(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	// The project's default is yolo: a mode raised to yolo in the window needs no decision.
	changeBeforeClaim(t, e, func() { execSQL(t, s, "UPDATE attempts SET mode='yolo' WHERE id='attempt'") })
	e.LaunchAttempt("attempt")
	waitAttemptID(t, s, "attempt", "ended_ok", "ended_failed")
	starts := strings.Join(markerLines(t, marker), "\n")
	if !strings.Contains(starts, `"mode":"yolo"`) || strings.Contains(starts, `"mode":"auto"`) {
		t.Fatalf("the engine received %s", starts)
	}
}

func TestAChangeInTheClaimsWindowThatNeedsADecisionLaunchesNothing(t *testing.T) {
	decided := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		setup  func(*testing.T, *store.Store)
		change func(*testing.T, *store.Store, *testClock)
	}{
		"a mode above the default": {
			setup: func(t *testing.T, s *store.Store) {
				execSQL(t, s, "UPDATE projects SET default_mode='auto' WHERE id='p'")
			},
			change: func(t *testing.T, s *store.Store, _ *testClock) {
				execSQL(t, s, "UPDATE attempts SET mode='yolo' WHERE id='attempt'")
			},
		},
		"an expiry": {
			setup: func(t *testing.T, s *store.Store) {
				readonlyDefault(t, s)
				decide(t, s, "u", "attempt", "allow", "allow", decided)
			},
			change: func(_ *testing.T, _ *store.Store, c *testClock) { c.set(decided.Add(store.DecisionLifetime)) },
		},
		"a lowered default": {
			setup: func(t *testing.T, s *store.Store) {
				execSQL(t, s, "UPDATE projects SET default_mode='auto' WHERE id='p'")
			},
			change: func(t *testing.T, s *store.Store, _ *testClock) { readonlyDefault(t, s) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, s, _, marker := testExecutor(t, "happy")
			e.cfg.CancelGrace = naturalExitGrace
			clock := &testClock{now: decided.Add(time.Hour)}
			e.cfg.Clock = clock.Now
			c.setup(t, s)
			claimed := make(chan struct{})
			e.beforeClaim = func(string) {
				select {
				case <-claimed:
				default:
					c.change(t, s, clock)
					close(claimed)
				}
			}
			e.LaunchAttempt("attempt")
			<-claimed
			staysUnlaunched(t, e, s, marker, "attempt")
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the engine started: %v", err)
			}
		})
	}
}

// verifyingTask leaves the fixture's task verifying with a check whose process tree runs.
func verifyingTask(t *testing.T) (*Executor, *store.Store, string) {
	t.Helper()
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	childMarker := filepath.Join(t.TempDir(), "verifying-child")
	cleanupGrandchild(t, childMarker)
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "running-check", Argv: checkHelperArgv("timeout_tree", childMarker)},
		{Name: "later-check", Argv: checkHelperArgv("pass")},
	})
	e.LaunchAttempt("attempt")
	waitTaskState(t, s, store.Verifying)
	waitGrandchildMarker(t, childMarker)
	return e, s, childMarker
}

// checkEndedWith waits for the verification to end and checks its check was killed and the
// attempt carries reason.
func checkEndedWith(t *testing.T, s *store.Store, reason string) {
	t.Helper()
	var task store.Task
	deadline := time.Now().Add(preparationBound)
	for {
		// Read as the other owner: the removed actor reads nothing of the project.
		var err error
		if task, err = s.Task(context.Background(), "w", "task"); err == nil && task.State == store.AwaitingReview {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task = %+v: %v", task, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if results := task.Attempt.CheckResults(); len(results) != 2 || results[0].State != store.CheckKilled || results[1].State != store.CheckNotRun {
		t.Fatalf("results = %+v", results)
	}
	events, err := s.Events(context.Background(), "w", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "cancel" && strings.Contains(string(e.Payload), reason) {
			return
		}
	}
	t.Fatalf("no cancel with %q in %+v", reason, events)
}

// anotherOwner adds w as a second owner of the fixture's project.
func anotherOwner(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.CreateUser(context.Background(), "w", "walt"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), "p", "w", "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestTheStopCountsAndEndsATaskInVerificationWithItsReason(t *testing.T) {
	e, s, _ := verifyingTask(t)
	anotherOwner(t, s)
	ctx := context.Background()
	counts, err := s.StopCounts(ctx, e.cfg.Generation, "w")
	if err != nil || counts != (store.StopCounts{Running: 1, OthersRunning: 1}) {
		t.Fatalf("counts %+v: %v", counts, err)
	}
	const reason = "coordinator stopped by the owner"
	var result store.StopResult
	if _, err := s.Do(ctx, "w", "stop", "stop", nil, func(tx *store.Tx) ([]byte, error) {
		var err error
		result, err = tx.StopWork(ctx, e.cfg.Generation, reason)
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
	if result.Running != 1 || strings.Join(result.Attempts, ",") != "attempt" {
		t.Fatalf("stop = %+v", result)
	}
	e.StopWork()
	checkEndedWith(t, s, reason)
}

func TestARemovalEndsTheRemovedActorsTaskInVerificationWithItsReason(t *testing.T) {
	e, s, _ := verifyingTask(t)
	anotherOwner(t, s)
	ctx := context.Background()
	var removal store.Removal
	if _, err := s.Do(ctx, "w", "remove", "remove", nil, func(tx *store.Tx) ([]byte, error) {
		var err error
		removal, err = tx.RemoveMembership(ctx, "p", "u", "remove", time.Now())
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(removal.Running, ",") != "task" {
		t.Fatalf("removal = %+v", removal)
	}
	// What the API does with the removal: the executor's verification cancel.
	for _, task := range removal.Running {
		e.CancelTask(task)
	}
	checkEndedWith(t, s, store.RemovedReason)
}
