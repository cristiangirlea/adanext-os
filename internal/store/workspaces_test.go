package store

import (
	"errors"
	"strings"
	"testing"
)

// boundTask imports p1 for alice with snapshot s1, and creates task t1 bound
// to it with an attempt a1.
func boundTask(t *testing.T, s *Store) {
	t.Helper()
	users(t, s, "alice", "bob")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		if _, err := tx.RecordSnapshot(ctx, testSnapshot()); err != nil {
			return err
		}
		if err := tx.RequireSnapshot(ctx, "p1", "s1"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,created,updated,snapshot) VALUES('t1','p1','alice','o','c','preparing','now','now','s1')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAttempt(ctx, Attempt{ID: "a1", Task: "t1", Actor: "alice", Backend: "codex", Mode: "auto", Executor: "local", State: "pending"}); err != nil {
		t.Fatal(err)
	}
}

func testWorkspace() Workspace {
	return Workspace{Attempt: "a1", Project: "p1", Task: "t1", Snapshot: "s1", BaseCommit: strings.Repeat("c", 40), Branch: "ada/t1",
		Path: "/state/projects/p1/worktrees/a1", ManifestVersion: 1, ManifestHash: hashOf('f'), Executor: "local", Policy: "auto"}
}

func TestATaskIsBoundToASnapshotOfItsOwnProjectOnly(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	if err := importAs(t, s, "alice", "p2", "/other"); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error { return tx.RequireSnapshot(ctx, "p2", "s1") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project's snapshot: %v", err)
	}
	if err := s.write(ctx, "alice", func(tx *Tx) error { return tx.RequireSnapshot(ctx, "p1", "missing") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown snapshot: %v", err)
	}
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || task.Snapshot != "s1" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	var src TaskSource
	if err := s.write(ctx, "alice", func(tx *Tx) error { var err error; src, err = tx.TaskSource(ctx, "t1"); return err }); err != nil {
		t.Fatal(err)
	}
	if src != (TaskSource{Imported: true, Snapshot: "s1", BaseCommit: strings.Repeat("c", 40)}) {
		t.Fatalf("task source = %+v", src)
	}
}

func TestTheExecutorReadsAnAttemptsSourceAndSnapshotWithoutAMembership(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	got, err := s.AttemptSource(ctx, "a1")
	if err != nil || got.Project != "p1" || got.Task != "t1" || got.SourcePath != "/repo" || got.Snapshot == nil {
		t.Fatalf("attempt source = %+v, %v", got, err)
	}
	if snap := *got.Snapshot; snap.ID != "s1" || snap.Staged != (ArtifactRef{hashOf('a'), 10}) || len(snap.Untracked) != 2 || snap.Untracked[1].Mode != "100755" {
		t.Fatalf("snapshot = %+v", snap)
	}
	seed(t, s)
	if err := s.CreateAttempt(ctx, Attempt{ID: "seeded", Task: "t", Actor: "u", Backend: "codex", Mode: "auto", Executor: "local", State: "pending"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AttemptSource(ctx, "seeded"); err != nil || got.SourcePath != "" || got.Snapshot != nil {
		t.Fatalf("a project without a source: %+v, %v", got, err)
	}
	if _, err := s.AttemptSource(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
}

func TestAnAttemptRecordsOneWorkspace(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	record := func(w Workspace) error {
		return s.write(ctx, "alice", func(tx *Tx) error { _, err := tx.RecordWorkspace(ctx, w); return err })
	}
	if err := record(testWorkspace()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Workspace(ctx, "a1")
	if err != nil || got.Created == "" {
		t.Fatalf("workspace = %+v, %v", got, err)
	}
	want := testWorkspace()
	want.Created = got.Created
	if got != want {
		t.Fatalf("workspace = %+v, want %+v", got, want)
	}
	if err := record(testWorkspace()); err == nil {
		t.Fatal("a second workspace for the attempt was recorded")
	}
	bad := testWorkspace()
	bad.Attempt, bad.ManifestHash = "missing", "short"
	if err := record(bad); err == nil {
		t.Fatal("a workspace with no attempt and a short hash was recorded")
	}
	if _, err := s.Workspace(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
}

func TestAVersionFiveStoreMigratesForwardWithItsTasksUnbound(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	path := s.path
	for _, query := range []string{"DROP TABLE check_results", "DROP TABLE check_baselines", "DROP TABLE check_worktrees", "DROP TABLE clone_configs", "ALTER TABLE workspaces DROP COLUMN tested_commit", "ALTER TABLE tasks DROP COLUMN checks_json", "ALTER TABLE attempts DROP COLUMN max_turns", "ALTER TABLE attempts DROP COLUMN timeout", "ALTER TABLE attempts DROP COLUMN provider_changes", "ALTER TABLE source_snapshots DROP COLUMN provider_configuration", "DROP TABLE snapshot_paths", "ALTER TABLE source_snapshots DROP COLUMN branch", "DROP TABLE workspaces", "ALTER TABLE tasks DROP COLUMN snapshot", "DELETE FROM schema_migrations WHERE version>=6", "PRAGMA user_version=5"} {
		execute(t, s, query)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	task, err := upgraded.Task(ctx, "u", "t")
	if err != nil || task.Snapshot != "" {
		t.Fatalf("an older task: %+v, %v", task, err)
	}
}
