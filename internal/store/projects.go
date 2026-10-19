package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrAlreadyImported = errors.New("source already imported")
	ErrNoSource        = errors.New("project has no imported source")
)

// AlreadyImportedError names the project that holds the source, and its
// tasks, only to a member of it; anyone else learns only that it exists.
type AlreadyImportedError struct {
	Project string
	Tasks   []TaskSummary
}

func (e *AlreadyImportedError) Error() string { return ErrAlreadyImported.Error() }
func (e *AlreadyImportedError) Unwrap() error { return ErrAlreadyImported }

type TaskSummary struct {
	ID, Objective string
	State         State
}

// ProjectSource is an imported project's source identity at import.
type ProjectSource struct {
	Project, Path, CommonDir, Fingerprint, Imported string
}

// ImportProject creates project id for src with the transaction's user as
// its owner and returns the source as recorded. A source whose canonical
// path a project already holds is refused with *AlreadyImportedError.
func (tx *Tx) ImportProject(ctx context.Context, id, name string, src ProjectSource) (ProjectSource, error) {
	var existing string
	err := tx.tx.QueryRowContext(ctx, "SELECT project FROM project_sources WHERE canonical_path=?", src.Path).Scan(&existing)
	if err == nil {
		return ProjectSource{}, alreadyImported(ctx, tx, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ProjectSource{}, err
	}
	at := now()
	for _, insert := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO projects(id,name,source_path,created) VALUES(?,?,?,?)", []any{id, name, src.Path, at}},
		{"INSERT INTO project_sources(project,canonical_path,common_dir,fingerprint,imported) VALUES(?,?,?,?,?)", []any{id, src.Path, src.CommonDir, src.Fingerprint, at}},
		{"INSERT INTO project_members(project,user,role,added) VALUES(?,?,'owner',?)", []any{id, tx.userID, at}},
	} {
		if _, err := tx.tx.ExecContext(ctx, insert.query, insert.args...); err != nil {
			return ProjectSource{}, err
		}
	}
	src.Project, src.Imported = id, at
	return src, nil
}

func alreadyImported(ctx context.Context, tx *Tx, project string) error {
	if err := requireMember(ctx, tx, tx.userID, project); errors.Is(err, ErrNotMember) {
		return &AlreadyImportedError{}
	} else if err != nil {
		return err
	}
	rows, err := tx.tx.QueryContext(ctx, "SELECT id,objective,state FROM tasks WHERE project=? ORDER BY created,id", project)
	if err != nil {
		return err
	}
	defer rows.Close()
	e := &AlreadyImportedError{Project: project, Tasks: []TaskSummary{}}
	for rows.Next() {
		var task TaskSummary
		if err := rows.Scan(&task.ID, &task.Objective, &task.State); err != nil {
			return err
		}
		e.Tasks = append(e.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return e
}

// ProjectSource is the project's source for a member: ErrNotFound for an
// unknown or inaccessible project alike, ErrNoSource for a member's project
// that was not imported.
func (s *Store) ProjectSource(ctx context.Context, userID, projectID string) (ProjectSource, error) {
	src := ProjectSource{Project: projectID}
	var path, commonDir, fingerprint, imported sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT s.canonical_path,s.common_dir,s.fingerprint,s.imported FROM project_members m
		LEFT JOIN project_sources s ON s.project=m.project WHERE m.project=? AND m.user=?`, projectID, userID).Scan(&path, &commonDir, &fingerprint, &imported)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectSource{}, ErrNotFound
	}
	if err != nil {
		return ProjectSource{}, err
	}
	if !path.Valid {
		return ProjectSource{}, ErrNoSource
	}
	src.Path, src.CommonDir, src.Fingerprint, src.Imported = path.String, commonDir.String, fingerprint.String, imported.String
	return src, nil
}

type ArtifactRef struct {
	Hash string
	Size int64
}

type SnapshotFile struct {
	Path, Mode string
	Artifact   ArtifactRef
}

type Snapshot struct {
	ID, Project, BaseCommit, Fingerprint string
	Staged, Unstaged                     ArtifactRef
	Untracked                            []SnapshotFile
	CreatedBy, Created                   string
}

// RecordSnapshot records a snapshot of an imported project for the
// transaction's user, a member, and references its artifacts, which the
// caller has already written.
func (tx *Tx) RecordSnapshot(ctx context.Context, snap Snapshot) (Snapshot, error) {
	if err := requireMember(ctx, tx, tx.userID, snap.Project); err != nil {
		return Snapshot{}, err
	}
	var one int
	if err := tx.tx.QueryRowContext(ctx, "SELECT 1 FROM project_sources WHERE project=?", snap.Project).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNoSource
	} else if err != nil {
		return Snapshot{}, err
	}
	reference := func(ref ArtifactRef, what string) error {
		return tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, fmt.Sprintf("snapshot:%s:%s", snap.ID, what))
	}
	if err := reference(snap.Staged, "staged"); err != nil {
		return Snapshot{}, err
	}
	if err := reference(snap.Unstaged, "unstaged"); err != nil {
		return Snapshot{}, err
	}
	snap.CreatedBy, snap.Created = tx.userID, now()
	if _, err := tx.tx.ExecContext(ctx, "INSERT INTO source_snapshots(id,project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created) VALUES(?,?,?,?,?,?,?,?)",
		snap.ID, snap.Project, snap.BaseCommit, snap.Fingerprint, snap.Staged.Hash, snap.Unstaged.Hash, snap.CreatedBy, snap.Created); err != nil {
		return Snapshot{}, err
	}
	for _, f := range snap.Untracked {
		if err := reference(f.Artifact, "untracked:"+f.Path); err != nil {
			return Snapshot{}, err
		}
		if _, err := tx.tx.ExecContext(ctx, "INSERT INTO snapshot_untracked(snapshot,path,mode,artifact) VALUES(?,?,?,?)", snap.ID, f.Path, f.Mode, f.Artifact.Hash); err != nil {
			return Snapshot{}, err
		}
	}
	return snap, nil
}

// Snapshot reads a snapshot of projectID for a member; an unknown snapshot
// and an inaccessible project are both ErrNotFound. A snapshot and its files
// commit together and never change, so the two reads need no transaction.
func (s *Store) Snapshot(ctx context.Context, userID, projectID, snapshotID string) (Snapshot, error) {
	return s.readSnapshot(ctx, "JOIN project_members m ON m.project=s.project AND m.user=?", []any{userID}, snapshotID, projectID)
}

// readSnapshot reads a snapshot of projectID; join, with its arguments,
// limits who may read it, and is empty for the trusted executor.
func (s *Store) readSnapshot(ctx context.Context, join string, joinArgs []any, snapshotID, projectID string) (Snapshot, error) {
	snap := Snapshot{ID: snapshotID, Project: projectID, Untracked: []SnapshotFile{}}
	err := s.db.QueryRowContext(ctx, `SELECT s.base_commit,s.fingerprint,s.staged_patch,staged.size,s.unstaged_patch,unstaged.size,s.created_by,s.created
		FROM source_snapshots s `+join+`
		JOIN artifacts staged ON staged.hash=s.staged_patch JOIN artifacts unstaged ON unstaged.hash=s.unstaged_patch
		WHERE s.id=? AND s.project=?`, append(joinArgs, snapshotID, projectID)...).Scan(&snap.BaseCommit, &snap.Fingerprint, &snap.Staged.Hash, &snap.Staged.Size, &snap.Unstaged.Hash, &snap.Unstaged.Size, &snap.CreatedBy, &snap.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT f.path,f.mode,f.artifact,a.size FROM snapshot_untracked f JOIN artifacts a ON a.hash=f.artifact WHERE f.snapshot=? ORDER BY f.rowid", snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var f SnapshotFile
		if err := rows.Scan(&f.Path, &f.Mode, &f.Artifact.Hash, &f.Artifact.Size); err != nil {
			return Snapshot{}, err
		}
		snap.Untracked = append(snap.Untracked, f)
	}
	return snap, rows.Err()
}
