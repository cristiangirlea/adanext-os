package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// prepare returns the engine's working directory and whether it is a
// workspace: for an imported project, a worktree of Ada's clone with the
// task's snapshot committed, recorded with its manifest; for a project with no
// imported source (only test seeds have none), the scratch directory.
// Preparation stops when the attempt is canceled or the coordinator stops.
func (e *Executor) prepare(a store.Attempt, active *active, scratch string) (string, bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-active.cancel:
			cancel()
		case <-ctx.Done():
		}
	}()
	src, err := e.cfg.Store.AttemptSource(ctx, a.ID)
	if err != nil {
		return "", false, err
	}
	if src.SourcePath == "" {
		if err := os.MkdirAll(scratch, 0700); err != nil {
			return "", false, fmt.Errorf("scratch directory: %w", err)
		}
		return scratch, false, os.Chmod(scratch, 0700)
	}
	if src.Snapshot == nil {
		return "", false, errors.New("snapshot_required: the task is bound to no snapshot")
	}
	if e.cfg.Workspaces == nil {
		return "", false, errors.New("git is unavailable, so no workspace can be prepared")
	}
	changes, err := e.snapshotChanges(*src.Snapshot)
	if err != nil {
		return "", false, err
	}
	m, err := e.cfg.Workspaces.Prepare(ctx, src.SourcePath, e.expectedManifest(a, src), changes)
	if err != nil {
		return "", false, err
	}
	err = e.owned(a.ID, func(tx *store.Tx) error {
		_, err := tx.RecordWorkspace(ctx, store.Workspace{Attempt: m.Attempt, Project: m.Project, Task: m.Task, Snapshot: m.Snapshot, BaseCommit: m.BaseCommit,
			Branch: m.Branch, Path: m.Path, ManifestVersion: m.Version, ManifestHash: m.Hash(), Executor: m.Executor, Policy: m.Policy})
		return err
	})
	return m.Path, true, err
}

// expectedManifest is the manifest the attempt's identity calls for. Prepare
// fills in the version, branch and path itself; checkWorkspace expects these.
func (e *Executor) expectedManifest(a store.Attempt, src store.AttemptSource) source.Manifest {
	m := source.Manifest{Project: src.Project, Task: src.Task, Attempt: a.ID, Executor: a.Executor, Policy: a.Mode}
	if src.Snapshot != nil {
		m.Snapshot, m.BaseCommit = src.Snapshot.ID, src.Snapshot.BaseCommit
	}
	return m
}

// snapshotChanges reads a snapshot's patches and files from their artifacts,
// each read to its end, where the artifact store checks its hash.
func (e *Executor) snapshotChanges(snap store.Snapshot) (source.Changes, error) {
	read := func(ref store.ArtifactRef) ([]byte, error) {
		if e.cfg.Artifacts == nil {
			return nil, errors.New("artifact store unavailable")
		}
		r, err := e.cfg.Artifacts.Open(ref.Hash)
		if err != nil {
			return nil, fmt.Errorf("snapshot artifact %s: %w", ref.Hash, err)
		}
		defer r.Close()
		data, err := io.ReadAll(io.LimitReader(r, ref.Size+1))
		if err == nil && int64(len(data)) != ref.Size {
			err = fmt.Errorf("%d bytes, recorded %d", len(data), ref.Size)
		}
		if err == nil {
			// The store checks the hash only at EOF, which a read of exactly Size bytes may not reach.
			_, err = io.Copy(io.Discard, r)
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot artifact %s: %w", ref.Hash, err)
		}
		return data, nil
	}
	var c source.Changes
	var err error
	if c.Staged, err = read(snap.Staged); err != nil {
		return c, err
	}
	if c.Unstaged, err = read(snap.Unstaged); err != nil {
		return c, err
	}
	for _, f := range snap.Untracked {
		content, err := read(f.Artifact)
		if err != nil {
			return c, err
		}
		c.Untracked = append(c.Untracked, source.File{Path: f.Path, Mode: f.Mode, Content: content})
	}
	return c, nil
}

// checkWorkspace compares the recorded workspace with the attempt's identity,
// and the worktree and its manifest copy with the record; it returns what
// differs, or "" when nothing does.
func (e *Executor) checkWorkspace(a store.Attempt) string {
	ctx := context.Background()
	src, err := e.cfg.Store.AttemptSource(ctx, a.ID)
	if err != nil {
		return "the attempt's source cannot be read: " + err.Error()
	}
	row, err := e.cfg.Store.Workspace(ctx, a.ID)
	if err != nil {
		return "the workspace row cannot be read: " + err.Error()
	}
	want := e.expectedManifest(a, src)
	want.Version, want.Branch, want.Path = source.ManifestVersion, "ada/"+src.Task, e.cfg.Workspaces.WorktreePath(src.Project, a.ID)
	recorded := source.Manifest{Version: row.ManifestVersion, Project: row.Project, Task: row.Task, Attempt: row.Attempt, Snapshot: row.Snapshot,
		BaseCommit: row.BaseCommit, Branch: row.Branch, Path: row.Path, Executor: row.Executor, Policy: row.Policy}
	var differ []string
	for _, f := range []struct{ name, recorded, want string }{
		{"version", strconv.Itoa(recorded.Version), strconv.Itoa(want.Version)}, {"project", recorded.Project, want.Project},
		{"task", recorded.Task, want.Task}, {"attempt", recorded.Attempt, want.Attempt}, {"snapshot", recorded.Snapshot, want.Snapshot},
		{"base commit", recorded.BaseCommit, want.BaseCommit}, {"branch", recorded.Branch, want.Branch}, {"path", recorded.Path, want.Path},
		{"executor", recorded.Executor, want.Executor}, {"policy", recorded.Policy, want.Policy},
	} {
		if f.recorded != f.want {
			differ = append(differ, f.name)
		}
	}
	if len(differ) > 0 {
		return "the workspace's " + strings.Join(differ, ", ") + " differ from the attempt's"
	}
	if err := e.cfg.Workspaces.Check(recorded, row.ManifestHash); err != nil {
		return err.Error()
	}
	return ""
}

// recoverWorkspace moves an attempt whose workspace no longer matches its
// manifest to recovery_required with a recovery event naming the mismatch.
// The engine has not started, and nothing is rebuilt.
func (e *Executor) recoverWorkspace(a store.Attempt, mismatch string) {
	payload, _ := json.Marshal(map[string]string{"reason": "workspace manifest mismatch", "mismatch": mismatch})
	err := e.artifactLifecycle(func() error {
		return e.owned(a.ID, func(tx *store.Tx) error {
			ctx := context.Background()
			var state store.State
			var rev int64
			if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
				return err
			}
			if err := tx.TransitionOwned(ctx, a.Task, state, store.Interrupted, rev); err != nil {
				return err
			}
			if err := tx.TransitionOwned(ctx, a.Task, store.Interrupted, store.RecoveryRequired, rev+1); err != nil {
				return err
			}
			at := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, "UPDATE attempts SET state='recovery_required',ended=? WHERE id=?", at, a.ID); err != nil {
				return err
			}
			var seq int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&seq); err != nil {
				return err
			}
			return e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: at, Kind: "recovery", Payload: payload})
		})
	})
	if err != nil {
		e.fail(a, fmt.Errorf("workspace recovery (%s): %w", mismatch, err))
		return
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}
