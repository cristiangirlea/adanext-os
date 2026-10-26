package executor

import (
	"bytes"
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
		if _, err := tx.RecordWorkspace(ctx, store.Workspace{Attempt: m.Attempt, Project: m.Project, Task: m.Task, Snapshot: m.Snapshot, BaseCommit: m.BaseCommit,
			Branch: m.Branch, Path: m.Path, ManifestVersion: m.Version, TaskRevision: m.TaskRevision, Generation: m.Generation, ManifestHash: m.Hash(), Executor: m.Executor, Policy: m.Policy}); err != nil {
			return err
		}
		return tx.RecordCloneConfig(ctx, m.Project, m.ConfigSHA256)
	})
	return m.Path, true, err
}

// expectedManifest is the manifest the attempt's identity calls for. Prepare
// fills in the version, branch and path itself; checkWorkspace expects these.
func (e *Executor) expectedManifest(a store.Attempt, src store.AttemptSource) source.Manifest {
	m := source.Manifest{Project: src.Project, Task: src.Task, TaskRevision: src.TaskRevision, Attempt: a.ID, Generation: a.OwnershipGeneration, Executor: a.Executor, Policy: a.Mode}
	if src.Snapshot != nil {
		m.Snapshot, m.BaseCommit = src.Snapshot.ID, src.Snapshot.BaseCommit
	}
	return m
}

// artifactMismatch is a snapshot artifact that is missing or no longer
// holds its recorded size and hash.
type artifactMismatch struct {
	hash string
	err  error
}

func (m *artifactMismatch) Error() string {
	return fmt.Sprintf("snapshot artifact %s: %v", m.hash, m.err)
}

// readArtifact copies a snapshot artifact to w, read to its end, where the
// artifact store checks its hash, and checks its recorded size.
func (e *Executor) readArtifact(ref store.ArtifactRef, w io.Writer) error {
	if e.cfg.Artifacts == nil {
		return errors.New("artifact store unavailable")
	}
	r, err := e.cfg.Artifacts.Open(ref.Hash)
	if err != nil {
		return &artifactMismatch{ref.Hash, err}
	}
	defer r.Close()
	n, err := io.Copy(w, io.LimitReader(r, ref.Size+1))
	if err == nil && n != ref.Size {
		err = fmt.Errorf("%d bytes, recorded %d", n, ref.Size)
	}
	if err == nil {
		// The store checks the hash only at EOF, which a read of exactly Size bytes may not reach.
		_, err = io.Copy(io.Discard, r)
	}
	if err != nil {
		return &artifactMismatch{ref.Hash, err}
	}
	return nil
}

// snapshotRefs are a snapshot's artifacts: its patches, then its files.
func snapshotRefs(snap store.Snapshot) []store.ArtifactRef {
	refs := []store.ArtifactRef{snap.Staged, snap.Unstaged}
	for _, f := range snap.Untracked {
		refs = append(refs, f.Artifact)
	}
	return refs
}

// snapshotChanges reads a snapshot's patches and files from their artifacts,
// each checked against its record before any of it is used.
func (e *Executor) snapshotChanges(snap store.Snapshot) (source.Changes, error) {
	var contents [][]byte
	for _, ref := range snapshotRefs(snap) {
		var buf bytes.Buffer
		if err := e.readArtifact(ref, &buf); err != nil {
			return source.Changes{}, err
		}
		contents = append(contents, buf.Bytes())
	}
	c := source.Changes{Staged: contents[0], Unstaged: contents[1]}
	for i, f := range snap.Untracked {
		c.Untracked = append(c.Untracked, source.File{Path: f.Path, Mode: f.Mode, Content: contents[2+i]})
	}
	return c, nil
}

// verifySnapshot reads every artifact of a snapshot again, keeping nothing.
func (e *Executor) verifySnapshot(snap store.Snapshot) error {
	for _, ref := range snapshotRefs(snap) {
		if err := e.readArtifact(ref, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

// Why a launch is refused before the engine starts.
const (
	manifestMismatch = "workspace manifest mismatch"
	snapshotMismatch = "snapshot artifact mismatch"
)

// checkWorkspace compares the recorded workspace with the attempt's identity,
// and the worktree and its manifest copy with the record, then reads the
// snapshot's artifacts again; it returns the reason and what differs, or ""
// when nothing does.
func (e *Executor) checkWorkspace(a store.Attempt) (reason, mismatch string) {
	ctx := context.Background()
	src, err := e.cfg.Store.AttemptSource(ctx, a.ID)
	if err != nil {
		return manifestMismatch, "the attempt's source cannot be read: " + err.Error()
	}
	row, err := e.cfg.Store.Workspace(ctx, a.ID)
	if err != nil {
		return manifestMismatch, "the workspace row cannot be read: " + err.Error()
	}
	want := e.expectedManifest(a, src)
	want.Version, want.Branch, want.Path = source.ManifestVersion, "ada/"+src.Task, e.cfg.Workspaces.WorktreePath(src.Project, a.ID)
	recorded := source.Manifest{Version: row.ManifestVersion, Project: row.Project, Task: row.Task, TaskRevision: row.TaskRevision, Attempt: row.Attempt,
		Generation: row.Generation, Snapshot: row.Snapshot, BaseCommit: row.BaseCommit, Branch: row.Branch, Path: row.Path, Executor: row.Executor, Policy: row.Policy}
	var differ []string
	for _, f := range []struct{ name, recorded, want string }{
		{"version", strconv.Itoa(recorded.Version), strconv.Itoa(want.Version)}, {"project", recorded.Project, want.Project},
		{"task", recorded.Task, want.Task}, {"task revision", strconv.FormatInt(recorded.TaskRevision, 10), strconv.FormatInt(want.TaskRevision, 10)},
		{"attempt", recorded.Attempt, want.Attempt}, {"ownership generation", strconv.FormatInt(recorded.Generation, 10), strconv.FormatInt(want.Generation, 10)},
		{"snapshot", recorded.Snapshot, want.Snapshot}, {"base commit", recorded.BaseCommit, want.BaseCommit}, {"branch", recorded.Branch, want.Branch},
		{"path", recorded.Path, want.Path}, {"executor", recorded.Executor, want.Executor}, {"policy", recorded.Policy, want.Policy},
	} {
		if f.recorded != f.want {
			differ = append(differ, f.name)
		}
	}
	if len(differ) > 0 {
		return manifestMismatch, "the workspace's " + strings.Join(differ, ", ") + " differ from the attempt's"
	}
	if err := e.cfg.Workspaces.Check(recorded, row.ManifestHash); err != nil {
		return manifestMismatch, err.Error()
	}
	if src.Snapshot == nil {
		return manifestMismatch, "the task has no snapshot"
	}
	if err := e.verifySnapshot(*src.Snapshot); err != nil {
		return snapshotMismatch, err.Error()
	}
	return "", ""
}

// recoverWorkspace moves an attempt whose launch was refused, its workspace
// or snapshot no longer matching what was recorded, to recovery_required
// with a recovery event naming the reason and the mismatch. The engine has
// not started, and nothing is rebuilt.
func (e *Executor) recoverWorkspace(a store.Attempt, reason, mismatch string) {
	payload, _ := json.Marshal(map[string]string{"reason": reason, "mismatch": mismatch})
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

// providerWalkLimit bounds each walk of a worktree for its provider configuration; past it the
// attempt is flagged as not fully assessed.
const providerWalkLimit = 200000

// recordProviderChanges compares the worktree's provider configuration with what it held before
// the engine started and records the difference on the attempt, so review and refine can show
// it (decisions log, 2026-10-25). A walk that failed, stopped at its limit or met a link at a
// provider path records the attempt as flagged regardless.
func (e *Executor) recordProviderChanges(a store.Attempt, work string, before map[string]string, unknown bool) {
	after, truncated, err := e.providerState(work, providerWalkLimit)
	changes := store.ProviderChanges{Truncated: unknown || truncated || err != nil}
	for _, c := range source.DiffProviderState(before, after) {
		changes.Changes = append(changes.Changes, store.ProviderChange{Path: c.Path, Change: c.Change})
	}
	e.saveProviderChanges(a, changes)
}

// saveProviderChanges records an assessment on the attempt and tells its subscribers.
func (e *Executor) saveProviderChanges(a store.Attempt, changes store.ProviderChanges) {
	if err := e.owned(a.ID, func(tx *store.Tx) error { return tx.RecordProviderChanges(context.Background(), a.ID, changes) }); err != nil {
		return
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}
