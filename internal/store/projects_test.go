package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func importAs(t *testing.T, s *Store, user, id, path string) error {
	t.Helper()
	return s.write(ctx, user, func(tx *Tx) error {
		src, err := tx.ImportProject(ctx, id, "name-"+id, ProjectSource{Path: path, CommonDir: path + "/.git", Fingerprint: "fp-" + id})
		if err == nil && (src.Project != id || src.Imported == "") {
			t.Errorf("recorded source = %+v", src)
		}
		return err
	})
}

func users(t *testing.T, s *Store, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := s.CreateUser(ctx, name, name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportCreatesTheProjectItsSourceAndTheOwnersMembership(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "alice")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	var name, sourcePath, role string
	if err := s.db.QueryRow("SELECT p.name,p.source_path,m.role FROM projects p JOIN project_members m ON m.project=p.id AND m.user='alice' WHERE p.id='p1'").Scan(&name, &sourcePath, &role); err != nil {
		t.Fatal(err)
	}
	if name != "name-p1" || sourcePath != "/repo" || role != "owner" {
		t.Fatalf("project %q %q %q", name, sourcePath, role)
	}
	src, err := s.ProjectSource(ctx, "alice", "p1")
	if err != nil || src.Path != "/repo" || src.CommonDir != "/repo/.git" || src.Fingerprint != "fp-p1" || src.Imported == "" {
		t.Fatalf("source = %+v, %v", src, err)
	}
}

func TestASecondImportOfTheSamePathNamesTheProjectAndItsTasksToAMemberOnly(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "alice", "bob")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "alice", "t1", "p1", "first", "done"); err != nil {
		t.Fatal(err)
	}
	err := importAs(t, s, "alice", "p2", "/repo")
	var already *AlreadyImportedError
	if !errors.As(err, &already) || !errors.Is(err, ErrAlreadyImported) || already.Project != "p1" || len(already.Tasks) != 1 || already.Tasks[0] != (TaskSummary{ID: "t1", Objective: "first", State: Draft}) {
		t.Fatalf("member: %v %+v", err, already)
	}
	err = importAs(t, s, "bob", "p3", "/repo")
	if !errors.As(err, &already) || already.Project != "" || already.Tasks != nil {
		t.Fatalf("non-member: %v %+v", err, already)
	}
	var projects int
	if err := s.db.QueryRow("SELECT count(*) FROM projects").Scan(&projects); err != nil || projects != 1 {
		t.Fatalf("projects %d: %v", projects, err)
	}
}

func TestProjectSourceIsForMembersAndSaysWhenThereIsNone(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	users(t, s, "bob")
	if _, err := s.ProjectSource(ctx, "u", "p"); !errors.Is(err, ErrNoSource) {
		t.Fatalf("project without a source: %v", err)
	}
	if _, err := s.ProjectSource(ctx, "bob", "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member: %v", err)
	}
	if _, err := s.ProjectSource(ctx, "u", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func hashOf(c byte) string { return strings.Repeat(string(c), 64) }

func testSnapshot() Snapshot {
	return Snapshot{
		ID: "s1", Project: "p1", BaseCommit: strings.Repeat("c", 40), Fingerprint: "fp",
		Staged: ArtifactRef{hashOf('a'), 10}, Unstaged: ArtifactRef{hashOf('b'), 0},
		Untracked: []SnapshotFile{{Path: "u/x.bin", Mode: "100644", Artifact: ArtifactRef{hashOf('d'), 3}}, {Path: "run.sh", Mode: "100755", Artifact: ArtifactRef{hashOf('e'), 4}}},
	}
}

func TestRecordSnapshotReferencesItsArtifactsAndReadsBackForMembersOnly(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "alice", "bob")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	var recorded Snapshot
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		var err error
		recorded, err = tx.RecordSnapshot(ctx, testSnapshot())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if recorded.CreatedBy != "alice" || recorded.Created == "" {
		t.Fatalf("recorded = %+v", recorded)
	}
	got, err := s.Snapshot(ctx, "alice", "p1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	want := testSnapshot()
	want.CreatedBy, want.Created = recorded.CreatedBy, recorded.Created
	if got.ID != want.ID || got.BaseCommit != want.BaseCommit || got.Staged != want.Staged || got.Unstaged != want.Unstaged || len(got.Untracked) != 2 || got.Untracked[0] != want.Untracked[0] || got.Untracked[1] != want.Untracked[1] || got.CreatedBy != "alice" {
		t.Fatalf("snapshot = %+v", got)
	}
	var origin string
	if err := s.db.QueryRow("SELECT origin FROM artifacts WHERE hash=?", hashOf('d')).Scan(&origin); err != nil || origin != "snapshot:s1:untracked:u/x.bin" {
		t.Fatalf("origin %q: %v", origin, err)
	}
	for _, read := range []struct{ user, project, id string }{{"bob", "p1", "s1"}, {"alice", "p2", "s1"}, {"alice", "p1", "missing"}} {
		if _, err := s.Snapshot(ctx, read.user, read.project, read.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: %v", read, err)
		}
	}
}

func TestRecordSnapshotRefusesANonMemberAndAProjectWithoutASourceAndStoresNothing(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	users(t, s, "alice", "bob")
	if err := importAs(t, s, "alice", "p1", "/repo"); err != nil {
		t.Fatal(err)
	}
	record := func(user string, snap Snapshot) error {
		return s.write(ctx, user, func(tx *Tx) error { _, err := tx.RecordSnapshot(ctx, snap); return err })
	}
	if err := record("bob", testSnapshot()); !errors.Is(err, ErrNotMember) {
		t.Fatalf("non-member: %v", err)
	}
	other := testSnapshot()
	other.Project = "p"
	if err := record("u", other); !errors.Is(err, ErrNoSource) {
		t.Fatalf("no source: %v", err)
	}
	bad := testSnapshot()
	bad.Untracked[1].Mode = "120000"
	if err := record("alice", bad); err == nil {
		t.Fatal("a symlink mode was recorded")
	}
	var snapshots, files, artifacts int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM source_snapshots),(SELECT count(*) FROM snapshot_untracked),(SELECT count(*) FROM artifacts)").Scan(&snapshots, &files, &artifacts); err != nil || snapshots+files+artifacts != 0 {
		t.Fatalf("stored %d snapshots, %d files, %d artifacts: %v", snapshots, files, artifacts, err)
	}
}

func TestAVersionFourStoreMigratesForwardKeepingItsProjects(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	path := s.path
	for _, query := range []string{"DROP TABLE check_results", "DROP TABLE check_baselines", "DROP TABLE check_worktrees", "ALTER TABLE workspaces DROP COLUMN tested_commit", "ALTER TABLE tasks DROP COLUMN checks_json", "ALTER TABLE attempts DROP COLUMN max_turns", "ALTER TABLE attempts DROP COLUMN timeout", "ALTER TABLE attempts DROP COLUMN provider_changes", "DROP TABLE snapshot_paths", "DROP TABLE workspaces", "ALTER TABLE tasks DROP COLUMN snapshot", "DROP TABLE snapshot_untracked", "DROP TABLE source_snapshots", "DROP TABLE project_sources", "DELETE FROM schema_migrations WHERE version>=5", "PRAGMA user_version=4"} {
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
	var version int
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 9 {
		t.Fatalf("version %d: %v", version, err)
	}
	if _, err := upgraded.ProjectSource(ctx, "u", "p"); !errors.Is(err, ErrNoSource) {
		t.Fatalf("an older project: %v", err)
	}
	if _, err := upgraded.Task(ctx, "u", "t"); err != nil {
		t.Fatalf("an older task: %v", err)
	}
}

func TestASnapshotKeepsItsBranchAndPathEntriesAndTheNewestIsReadForMembers(t *testing.T) {
	s := openTestStore(t)
	users(t, s, "alice", "bob")
	for _, p := range []string{"p1", "p2"} {
		if err := importAs(t, s, "alice", p, "/repo-"+p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.NewestSnapshot(ctx, "alice", "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no snapshot yet: %v", err)
	}
	first := testSnapshot()
	first.State = &SnapshotState{Branch: "main", Paths: []SnapshotPath{{Path: "a.txt", Entry: hashOf('1')}, {Path: "n.txt", Untracked: true, Entry: hashOf('2')}}}
	second := testSnapshot()
	second.ID, second.State = "s2", &SnapshotState{Branch: "", Paths: []SnapshotPath{}}
	legacy := testSnapshot()
	legacy.ID, legacy.Project = "s3", "p2"
	for _, snap := range []Snapshot{first, second, legacy} {
		if err := s.write(ctx, "alice", func(tx *Tx) error { _, err := tx.RecordSnapshot(ctx, snap); return err }); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Snapshot(ctx, "alice", "p1", "s1")
	if err != nil || got.State == nil || got.State.Branch != "main" || !slices.Equal(got.State.Paths, first.State.Paths) {
		t.Fatalf("first = %+v, %v", got, err)
	}
	newest, err := s.NewestSnapshot(ctx, "alice", "p1")
	if err != nil || newest.ID != "s2" || newest.State == nil || newest.State.Branch != "" || newest.State.Paths == nil || len(newest.State.Paths) != 0 {
		t.Fatalf("newest = %+v, %v", newest, err)
	}
	if got, err := s.NewestSnapshot(ctx, "alice", "p2"); err != nil || got.ID != "s3" || got.State != nil {
		t.Fatalf("a snapshot with no recorded state = %+v, %v", got, err)
	}
	if _, err := s.NewestSnapshot(ctx, "bob", "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member: %v", err)
	}
}

func TestAVersionSixStoreMigratesForwardWithItsSnapshotsAndWorkspacesUnbound(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	if err := s.write(ctx, "alice", func(tx *Tx) error { _, err := tx.RecordWorkspace(ctx, testWorkspace()); return err }); err != nil {
		t.Fatal(err)
	}
	path := s.path
	for _, query := range []string{"DROP TABLE check_results", "DROP TABLE check_baselines", "DROP TABLE check_worktrees", "ALTER TABLE workspaces DROP COLUMN tested_commit", "ALTER TABLE tasks DROP COLUMN checks_json", "ALTER TABLE attempts DROP COLUMN max_turns", "ALTER TABLE attempts DROP COLUMN timeout", "ALTER TABLE attempts DROP COLUMN provider_changes", "ALTER TABLE source_snapshots DROP COLUMN provider_configuration", "DROP TABLE snapshot_paths", "ALTER TABLE source_snapshots DROP COLUMN branch", "ALTER TABLE workspaces DROP COLUMN task_revision",
		"ALTER TABLE workspaces DROP COLUMN ownership_generation", "DELETE FROM schema_migrations WHERE version>=7", "PRAGMA user_version=6"} {
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
	if snap, err := upgraded.Snapshot(ctx, "alice", "p1", "s1"); err != nil || snap.State != nil {
		t.Fatalf("an older snapshot: %+v, %v", snap, err)
	}
	// An older row binds no revision or generation, so it matches no attempt.
	if w, err := upgraded.Workspace(ctx, "a1"); err != nil || w.TaskRevision != -1 || w.Generation != -1 {
		t.Fatalf("an older workspace: %+v, %v", w, err)
	}
}
