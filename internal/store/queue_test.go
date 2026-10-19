package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seedQueue leaves task t preparing with attempt a pending, as a start records.
func seedQueue(t *testing.T, s *Store) {
	t.Helper()
	seed(t, s)
	execute(t, s, "UPDATE tasks SET state='preparing',revision=2 WHERE id='t'")
	execute(t, s, "UPDATE attempts SET state='pending',ownership_generation=0,started='2026-10-15T10:00:00Z' WHERE id='a'")
}

func TestCancelQueuedClosesThePendingAttemptAndTheTaskInOneTransaction(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	var attemptID string
	err := s.write(ctx, "u", func(tx *Tx) error {
		var err error
		attemptID, err = tx.CancelQueued(ctx, "t", 2, "withdrawn by u")
		return err
	})
	if err != nil || attemptID != "a" {
		t.Fatalf("cancel queued = %q, %v", attemptID, err)
	}
	a, err := s.AttemptByID(ctx, "a")
	if err != nil || a.State != "canceled" || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	task, err := s.Task(ctx, "u", "t")
	if err != nil || task.State != Canceled || task.Revision != 3 {
		t.Fatalf("task = %+v: %v", task, err)
	}
	events, err := s.Events(ctx, "u", "a", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !strings.Contains(string(events[0].Payload), `"reason":"withdrawn by u"`) {
		t.Fatalf("events = %+v: %v", events, err)
	}
}

func TestPreparingToCanceledIsRefusedOnceTheAttemptLeftPending(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	// The edge alone, without closing the attempt, is refused while it is pending.
	if err := s.Transition(ctx, "u", "t", Preparing, Canceled, 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("bare transition = %v", err)
	}
	execute(t, s, "UPDATE attempts SET state='launching' WHERE id='a'")
	err := s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.CancelQueued(ctx, "t", 2, "withdrawn by u")
		return err
	})
	if !errors.Is(err, ErrAttemptLaunched) {
		t.Fatalf("cancel queued after launch = %v", err)
	}
	if err := s.Transition(ctx, "u", "t", Preparing, Canceled, 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("bare transition after launch = %v", err)
	}
	task, err := s.Task(ctx, "u", "t")
	if err != nil || task.State != Preparing || task.Revision != 2 {
		t.Fatalf("task = %+v: %v", task, err)
	}
	a, err := s.AttemptByID(ctx, "a")
	if err != nil || a.State != "launching" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
}

func TestACancelThatFindsTheAttemptLaunchingRecordsItForTheAccept(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	execute(t, s, "UPDATE attempts SET state='launching' WHERE id='a'")
	// The API commits the transaction that found the attempt launched.
	err := s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.CancelQueued(ctx, "t", 2, "withdrawn by u")
		if !errors.Is(err, ErrAttemptLaunched) {
			t.Errorf("cancel queued after launch = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, "u", "a", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !strings.Contains(string(events[0].Payload), `"reason":"withdrawn by u"`) {
		t.Fatalf("events = %+v: %v", events, err)
	}
	err = s.write(ctx, "u", func(tx *Tx) error {
		canceled, err := tx.CancelRecorded(ctx, "a")
		if err == nil && !canceled {
			t.Error("the accept would not see the member's cancel")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(ctx, "u", "t")
	if err != nil || task.State != Preparing || task.Revision != 2 {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestCancelQueuedNeedsTheRevisionAndMembership(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	execute(t, s, `INSERT INTO users VALUES ('v', 'other', 'created')`)
	err := s.write(ctx, "v", func(tx *Tx) error {
		_, err := tx.CancelQueued(ctx, "t", 2, "withdrawn by v")
		return err
	})
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("non-member = %v", err)
	}
	err = s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.CancelQueued(ctx, "t", 1, "withdrawn by u")
		return err
	})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision = %v", err)
	}
	a, err := s.AttemptByID(ctx, "a")
	if err != nil || a.State != "pending" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
}

func seedSecondUserAndTasks(t *testing.T, s *Store) {
	t.Helper()
	execute(t, s, `INSERT INTO users VALUES ('v', 'other', 'created')`)
	execute(t, s, `INSERT INTO project_members VALUES ('p', 'v', 'member', 'added')`)
	execute(t, s, `INSERT INTO projects VALUES ('q', 'other project', '/other', '2026-10-11T00:00:00Z')`)
	execute(t, s, `INSERT INTO project_members VALUES ('q', 'u', 'owner', 'added')`)
	execute(t, s, `INSERT INTO project_members VALUES ('q', 'v', 'member', 'added')`)
	for _, row := range [][3]string{{"t2", "p", "u"}, {"t3", "q", "v"}, {"t4", "p", "u"}} {
		execute(t, s, `INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated) VALUES (?, ?, ?, 'objective', 'criteria', 'preparing', 2, 'created', 'updated')`, row[0], row[1], row[2])
	}
	// Three more pending attempts: the second created in the same instant as
	// the first, so the row order breaks the tie; one by the other user.
	for _, row := range [][4]string{{"b", "t2", "v", "2026-10-15T10:00:00Z"}, {"c", "t3", "v", "2026-10-15T10:00:01Z"}, {"d", "t4", "u", "2026-10-15T10:00:02Z"}} {
		execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,executor,ownership_generation,engine_sha256,state,started) VALUES (?, ?, ?, 'codex', '', '', 'local', 0, 'hash', 'pending', ?)`, row[0], row[1], row[2], row[3])
	}
}

func TestQueuePositionCountsPendingAttemptsInCreationOrderAcrossUsers(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	seedSecondUserAndTasks(t, s)
	for task, want := range map[string]int{"t": 1, "t2": 2, "t3": 3, "t4": 4} {
		if got, err := s.QueuePosition(ctx, task); err != nil || got != want {
			t.Fatalf("position of %s = %d, %v, want %d", task, got, err, want)
		}
	}
	execute(t, s, "UPDATE attempts SET state='launching' WHERE id='a'")
	if got, err := s.QueuePosition(ctx, "t"); err != nil || got != 0 {
		t.Fatalf("position of a launched task = %d, %v", got, err)
	}
	if got, err := s.QueuePosition(ctx, "t3"); err != nil || got != 2 {
		t.Fatalf("position after the head launched = %d, %v", got, err)
	}
	if got, err := s.QueuePosition(ctx, "missing"); err != nil || got != 0 {
		t.Fatalf("position of an unknown task = %d, %v", got, err)
	}
}

func TestNextPendingAttemptFollowsCreationOrderAndSkipsBusyProjects(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	seedSecondUserAndTasks(t, s)
	var order []string
	for {
		a, ok, err := s.NextPendingAttempt(ctx, 0, func(string) bool { return false })
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		order = append(order, a.ID)
		execute(t, s, "UPDATE attempts SET state='launching' WHERE id=?", a.ID)
	}
	if strings.Join(order, ",") != "a,b,c,d" {
		t.Fatalf("launch order = %v", order)
	}
	execute(t, s, "UPDATE attempts SET state='pending'")
	a, ok, err := s.NextPendingAttempt(ctx, 0, func(project string) bool { return project == "p" })
	if err != nil || !ok || a.ID != "c" {
		t.Fatalf("next outside the busy project = %+v, %v, %v", a, ok, err)
	}
	if _, ok, err := s.NextPendingAttempt(ctx, 1, func(string) bool { return false }); err != nil || ok {
		t.Fatalf("another generation's attempt offered: %v, %v", ok, err)
	}
}

func TestTheQueueFollowsInsertionOrderWhateverTheFractionalSecondsOfStarted(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	seedSecondUserAndTasks(t, s)
	// RFC3339Nano drops trailing zeros, so as text ".12Z" sorts after ".123Z"
	// and ".1234Z": the creation order a, b, c, d read by started would be a,
	// d, c, b.
	for id, started := range map[string]string{"b": "2026-10-15T10:00:00.12Z", "c": "2026-10-15T10:00:00.123Z", "d": "2026-10-15T10:00:00.1234Z"} {
		execute(t, s, "UPDATE attempts SET started=? WHERE id=?", started, id)
	}
	var order []string
	for {
		a, ok, err := s.NextPendingAttempt(ctx, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		order = append(order, a.ID)
		execute(t, s, "UPDATE attempts SET state='launching' WHERE id=?", a.ID)
	}
	if strings.Join(order, ",") != "a,b,c,d" {
		t.Fatalf("launch order = %v", order)
	}
	execute(t, s, "UPDATE attempts SET state='pending'")
	for task, want := range map[string]int{"t": 1, "t2": 2, "t3": 3, "t4": 4} {
		if got, err := s.QueuePosition(ctx, task); err != nil || got != want {
			t.Fatalf("position of %s = %d, %v, want %d", task, got, err, want)
		}
	}
}

func TestStopWorkWithdrawsQueuedAttemptsAndRequestsCancellationOfRunningOnes(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	seedSecondUserAndTasks(t, s)
	execute(t, s, "UPDATE tasks SET state='running',revision=3 WHERE id='t'")
	execute(t, s, "UPDATE attempts SET state='running' WHERE id='a'")
	execute(t, s, "UPDATE attempts SET state='launching' WHERE id='b'")
	// t4's actor has already asked for a cancel: the stop adds its reason and
	// leaves that request as it is.
	execute(t, s, "UPDATE tasks SET state='cancel_requested',revision=4 WHERE id='t4'")
	execute(t, s, "UPDATE attempts SET state='running' WHERE id='d'")
	var result StopResult
	err := s.write(ctx, "local", func(tx *Tx) error {
		var err error
		result, err = tx.StopWork(ctx, 0, "coordinator stopped by the owner")
		return err
	})
	if err != nil || result.Running != 3 || result.Queued != 1 || strings.Join(result.Attempts, ",") != "a,b,c,d" {
		t.Fatalf("stop work = %+v, %v", result, err)
	}
	for id, want := range map[string]string{"a": "running", "b": "launching", "c": "canceled", "d": "running"} {
		a, err := s.AttemptByID(ctx, id)
		if err != nil || a.State != want {
			t.Fatalf("attempt %s = %+v: %v, want %s", id, a, err, want)
		}
		actor := a.Actor
		events, err := s.Events(ctx, actor, id, 0)
		if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !strings.Contains(string(events[0].Payload), `"reason":"coordinator stopped by the owner"`) {
			t.Fatalf("events of %s as %s = %+v: %v", id, actor, events, err)
		}
	}
	// A running task is durably cancel_requested in the stop's own transaction,
	// so a result the engine returns meanwhile cannot end it ok; a launching
	// one is still preparing, and the executor honours the reason on accept.
	for task, want := range map[string]struct {
		state    State
		revision int64
	}{"t": {CancelRequested, 4}, "t2": {Preparing, 2}, "t3": {Canceled, 3}, "t4": {CancelRequested, 4}} {
		got, err := s.Task(ctx, "u", task)
		if err != nil || got.State != want.state || got.Revision != want.revision {
			t.Fatalf("task %s = %+v: %v, want %+v", task, got, err, want)
		}
	}
}

func TestRestartRecoversAQueuedAttemptBehindARunningOneWithoutRelaunch(t *testing.T) {
	s := openTestStore(t)
	seedQueue(t, s)
	seedSecondUserAndTasks(t, s)
	execute(t, s, "UPDATE tasks SET state='running',revision=3 WHERE id='t'")
	execute(t, s, "UPDATE attempts SET state='running' WHERE id='a'")
	generation, ids, err := s.BumpAndRecover(context.Background())
	if err != nil || generation != 1 || len(ids) != 4 {
		t.Fatalf("recovery = %d %v %v", generation, ids, err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		a, err := s.AttemptByID(ctx, id)
		if err != nil || a.State != "recovery_required" {
			t.Fatalf("attempt %s = %+v: %v", id, a, err)
		}
	}
	events, err := s.Events(ctx, "u", "b", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "recovery" || !strings.Contains(string(events[0].Payload), `"attempt_state":"pending"`) {
		t.Fatalf("queued attempt's recovery event = %+v: %v", events, err)
	}
	if _, ok, err := s.NextPendingAttempt(ctx, generation, func(string) bool { return false }); err != nil || ok {
		t.Fatalf("a recovered attempt is offered for launch: %v, %v", ok, err)
	}
}
