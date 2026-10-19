package store

import (
	"context"
	"database/sql"
	"errors"
)

// RequireSnapshot refuses, with ErrNotFound, a snapshot that is not one of
// the project's; the caller has already checked the user's membership.
func (tx *Tx) RequireSnapshot(ctx context.Context, project, snapshot string) error {
	var one int
	err := tx.tx.QueryRowContext(ctx, "SELECT 1 FROM source_snapshots WHERE id=? AND project=?", snapshot, project).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// TaskSource is what a start needs to know of a task's repository: whether
// its project was imported, and the snapshot the task is bound to with that
// snapshot's base commit (empty for an unborn branch).
type TaskSource struct {
	Imported             bool
	Snapshot, BaseCommit string
}

func (tx *Tx) TaskSource(ctx context.Context, taskID string) (TaskSource, error) {
	var src TaskSource
	var snapshot, base sql.NullString
	err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_sources p WHERE p.project=t.project),t.snapshot,s.base_commit
		FROM tasks t LEFT JOIN source_snapshots s ON s.id=t.snapshot WHERE t.id=?`, taskID).Scan(&src.Imported, &snapshot, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskSource{}, ErrNotFound
	}
	src.Snapshot, src.BaseCommit = snapshot.String, base.String
	return src, err
}

// AttemptSource is what the trusted executor prepares an attempt from: its
// task's project and that project's imported path, empty when it has none,
// and the task's snapshot, nil when it has none.
type AttemptSource struct {
	Project, Task, SourcePath string
	Snapshot                  *Snapshot
}

func (s *Store) AttemptSource(ctx context.Context, attemptID string) (AttemptSource, error) {
	var src AttemptSource
	var path, snapshot sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT t.project,t.id,p.canonical_path,t.snapshot FROM attempts a JOIN tasks t ON t.id=a.task
		LEFT JOIN project_sources p ON p.project=t.project WHERE a.id=?`, attemptID).Scan(&src.Project, &src.Task, &path, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptSource{}, ErrNotFound
	}
	if err != nil {
		return AttemptSource{}, err
	}
	src.SourcePath = path.String
	if snapshot.Valid {
		snap, err := s.readSnapshot(ctx, "", nil, snapshot.String, src.Project)
		if err != nil {
			return AttemptSource{}, err
		}
		src.Snapshot = &snap
	}
	return src, nil
}

// Workspace is an attempt's worktree as recorded: its manifest's fields and
// the manifest's hash.
type Workspace struct {
	Attempt, Project, Task, Snapshot, BaseCommit, Branch, Path string
	ManifestVersion                                            int
	ManifestHash, Executor, Policy, Created                    string
}

const workspaceColumns = "attempt,project,task,snapshot,base_commit,branch,path,manifest_version,manifest_hash,executor,policy,created"

// RecordWorkspace records an attempt's prepared worktree; an attempt has one.
func (tx *Tx) RecordWorkspace(ctx context.Context, w Workspace) (Workspace, error) {
	w.Created = now()
	_, err := tx.tx.ExecContext(ctx, "INSERT INTO workspaces("+workspaceColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		w.Attempt, w.Project, w.Task, w.Snapshot, w.BaseCommit, w.Branch, w.Path, w.ManifestVersion, w.ManifestHash, w.Executor, w.Policy, w.Created)
	return w, err
}

// Workspace reads an attempt's recorded worktree for the trusted executor.
func (s *Store) Workspace(ctx context.Context, attemptID string) (Workspace, error) {
	var w Workspace
	err := s.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspaces WHERE attempt=?", attemptID).Scan(
		&w.Attempt, &w.Project, &w.Task, &w.Snapshot, &w.BaseCommit, &w.Branch, &w.Path, &w.ManifestVersion, &w.ManifestHash, &w.Executor, &w.Policy, &w.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return w, err
}
