package store

import (
	"testing"
)

func TestAStartsLimitsAreRecordedWithTheAttempt(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	if turns, timeout, err := s.AttemptLimits(ctx, "a1"); err != nil || turns != 0 || timeout != "" {
		t.Fatalf("an attempt without limits: %d %q %v", turns, timeout, err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET max_turns=3,timeout='3m' WHERE id='a1'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if turns, timeout, err := s.AttemptLimits(ctx, "a1"); err != nil || turns != 3 || timeout != "3m" {
		t.Fatalf("limits: %d %q %v", turns, timeout, err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET max_turns=-1 WHERE id='a1'")
		return err
	}); err == nil {
		t.Fatal("a negative turn limit was stored")
	}
}

func TestASnapshotKeepsItsProviderConfigurationAndTheTaskShowsIt(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "alice")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	snap := testSnapshot()
	snap.ProviderConfiguration = &ProviderConfiguration{Paths: []string{".claude/settings.json", ".mcp.json"}, Count: 2}
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		if _, err := tx.RecordSnapshot(ctx, snap); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,created,updated,snapshot) VALUES('t1','p1','alice','o','c','ready','now','now','s1')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	read, err := s.Snapshot(ctx, "alice", "p1", "s1")
	if err != nil || read.ProviderConfiguration == nil || read.ProviderConfiguration.Count != 2 || read.ProviderConfiguration.Paths[0] != ".claude/settings.json" {
		t.Fatalf("snapshot = %+v, %v", read.ProviderConfiguration, err)
	}
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || task.ProviderConfiguration == nil || task.ProviderConfiguration.Paths[1] != ".mcp.json" {
		t.Fatalf("task = %+v, %v", task.ProviderConfiguration, err)
	}
	tasks, err := s.ListTasks(ctx, "alice")
	if err != nil || len(tasks) != 1 || tasks[0].ProviderConfiguration == nil || len(tasks[0].ProviderConfiguration.Paths) != 2 {
		t.Fatalf("list = %+v, %v", tasks, err)
	}
}

func TestASnapshotWithoutProviderConfigurationShowsNoneRatherThanUnknown(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || task.ProviderConfiguration == nil || task.ProviderConfiguration.Count != 0 || len(task.ProviderConfiguration.Paths) != 0 {
		t.Fatalf("task = %#v, %v", task.ProviderConfiguration, err)
	}
}

func TestProviderChangesAreRecordedOnTheAttemptAndShownWithTheTask(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || task.Attempt.ProviderChanges != nil {
		t.Fatalf("before the assessment: %+v, %v", task.Attempt, err)
	}
	changes := ProviderChanges{Changes: []ProviderChange{{Path: ".claude/settings.json", Change: "altered"}}}
	if err := s.write(ctx, "alice", func(tx *Tx) error { return tx.RecordProviderChanges(ctx, "a1", changes) }); err != nil {
		t.Fatal(err)
	}
	task, err = s.Task(ctx, "alice", "t1")
	if err != nil || task.Attempt.ProviderChanges == nil || len(task.Attempt.ProviderChanges.Changes) != 1 || task.Attempt.ProviderChanges.Changes[0].Change != "altered" {
		t.Fatalf("after: %+v, %v", task.Attempt.ProviderChanges, err)
	}
	if !task.Attempt.ProviderChanges.Flagged() {
		t.Fatal("an altered settings file is not flagged")
	}
	if (ProviderChanges{}).Flagged() || !(ProviderChanges{Truncated: true}).Flagged() {
		t.Fatal("Flagged: nothing changed must not flag, an unfinished assessment must")
	}
}

func TestAFinishedAttemptWithAWorkspaceAndNoAssessmentIsFlagged(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	if err := s.write(ctx, "alice", func(tx *Tx) error { _, err := tx.RecordWorkspace(ctx, testWorkspace()); return err }); err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || !task.Attempt.Workspace || task.Attempt.ProviderFlagged() {
		t.Fatalf("a pending attempt: %+v, %v", task.Attempt, err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='ended_ok',ended='now' WHERE id='a1'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	task, err = s.Task(ctx, "alice", "t1")
	if err != nil || !task.Attempt.Workspace || task.Attempt.ProviderChanges != nil || !task.Attempt.ProviderFlagged() {
		t.Fatalf("a finished attempt: %+v, %v", task.Attempt, err)
	}
}
