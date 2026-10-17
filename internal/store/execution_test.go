package store

import (
	"context"
	"errors"
	"testing"
)

func TestPreparingCanFailOrInterruptWithoutOpeningOtherEdges(t *testing.T) {
	for _, to := range []State{Failed, Interrupted} {
		if !allowedTransition(Preparing, to) {
			t.Errorf("preparing -> %s refused", to)
		}
	}
	// Canceled is admitted by the edge table and guarded by transition itself
	// (queue_test.go): only with the pending attempt closed in the same transaction.
	for _, to := range []State{Verifying, RecoveryRequired, Ready} {
		if allowedTransition(Preparing, to) {
			t.Errorf("preparing -> %s allowed", to)
		}
	}
}

func TestVersionThreeAttemptMigratesForwardWithEmptyMode(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	path := s.path
	execute(t, s, "ALTER TABLE attempts DROP COLUMN mode")
	execute(t, s, "DROP TABLE coordinator")
	execute(t, s, "DROP TABLE snapshot_untracked")
	execute(t, s, "DROP TABLE source_snapshots")
	execute(t, s, "DROP TABLE project_sources")
	execute(t, s, "DELETE FROM schema_migrations WHERE version>=4")
	execute(t, s, "PRAGMA user_version=3")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var mode string
	if err := upgraded.db.QueryRow("SELECT mode FROM attempts WHERE id='a'").Scan(&mode); err != nil || mode != "" {
		t.Fatalf("mode = %q: %v", mode, err)
	}
}

func TestRestartRecoversPendingAndRunningAttemptsWithoutRelaunch(t *testing.T) {
	for _, state := range []string{"pending", "launching", "running"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			current := "preparing"
			if state == "running" {
				current = "running"
			}
			execute(t, s, "UPDATE tasks SET state=?,revision=2 WHERE id='t'", current)
			execute(t, s, "UPDATE attempts SET state=?,ownership_generation=0 WHERE id='a'", state)
			generation, ids, err := s.BumpAndRecover(context.Background())
			if err != nil || generation != 1 || len(ids) != 1 || ids[0] != "a" {
				t.Fatalf("recovery = %d %v %v", generation, ids, err)
			}
			a, err := s.AttemptByID(context.Background(), "a")
			if err != nil || a.State != "recovery_required" || a.Ended == "" {
				t.Fatalf("attempt = %+v: %v", a, err)
			}
			task, err := s.Task(context.Background(), "u", "t")
			if err != nil || task.State != RecoveryRequired {
				t.Fatalf("task = %+v: %v", task, err)
			}
			events, err := s.Events(context.Background(), "u", "a", 0)
			if err != nil || len(events) != 1 || events[0].Kind != "recovery" {
				t.Fatalf("events = %+v: %v", events, err)
			}
		})
	}
}

func TestOldGenerationCannotAppendAnEvent(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	execute(t, s, "UPDATE tasks SET state='running',revision=3 WHERE id='t'")
	_, _, err := s.BumpAndRecover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = s.DoOwned(context.Background(), "a", 0, func(tx *Tx) error {
		return tx.AppendOwnedEvent(context.Background(), Event{Attempt: "a", Seq: 1, Payload: []byte(`{}`)})
	})
	if !errors.Is(err, ErrStaleOwnership) {
		t.Fatalf("write = %v", err)
	}
	events, err := s.Events(context.Background(), "u", "a", 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("stale writer appended events = %+v: %v", events, err)
	}
}

func TestRestartRecoversACancellationWhoseOutcomeIsUnknown(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	execute(t, s, "UPDATE tasks SET state='cancel_requested',revision=4 WHERE id='t'")
	execute(t, s, "UPDATE attempts SET state='running',ownership_generation=0 WHERE id='a'")
	_, ids, err := s.BumpAndRecover(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("recovery = %v: %v", ids, err)
	}
	task, err := s.Task(context.Background(), "u", "t")
	if err != nil || task.State != RecoveryRequired {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestVersionFourAddsModeAndCoordinatorWithoutChangingOldAttempt(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	var version int
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatalf("version = %d: %v", version, err)
	}
	var mode string
	if err := s.db.QueryRowContext(context.Background(), "SELECT mode FROM attempts WHERE id='a'").Scan(&mode); err != nil || mode != "" {
		t.Fatalf("old attempt mode = %q: %v", mode, err)
	}
	var generation int64
	if err := s.db.QueryRowContext(context.Background(), "SELECT generation FROM coordinator WHERE id=1").Scan(&generation); err != nil || generation != 0 {
		t.Fatalf("generation = %d: %v", generation, err)
	}
}
