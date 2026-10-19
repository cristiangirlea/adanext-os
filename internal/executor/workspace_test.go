package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// fixtureGit runs the system git in dir to build a repository, with the
// machine's own system and global config left out.
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Ada Test", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada Test", "GIT_COMMITTER_EMAIL=ada@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeSums hashes every file under dir, .git included.
func treeSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		sums[path] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// preparationBound is the liveness bound for an attempt that prepares a
// workspace: a clone or fetch and a worktree with the snapshot committed, a
// few dozen git processes that a loaded Windows PC starts slowly.
const preparationBound = 120 * time.Second

func waitAttemptPrepared(t *testing.T, s *store.Store, want string) store.Attempt {
	t.Helper()
	return waitAttemptWithin(t, s, want, preparationBound)
}

// workspaceExecutor is testExecutor with project p imported from a
// repository holding staged, unstaged and untracked work, and the task bound
// to a snapshot of it. Its cleanup stops the executor before the temporary
// directories go, so no preparation's git is still running in them.
func workspaceExecutor(t *testing.T, script string) (e *Executor, s *store.Store, marker, repo, worktree string) {
	t.Helper()
	ctx := context.Background()
	e, s, dir, marker := testExecutor(t, script)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "init", "-q")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\n")
	fixtureGit(t, repo, "add", "a.txt")
	fixtureGit(t, repo, "commit", "-q", "-m", "first")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\nstaged\n")
	fixtureGit(t, repo, "add", "a.txt")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\nstaged\nunstaged\n")
	writeFile(t, filepath.Join(repo, "notes", "n.bin"), "\x00\x01note\r\n")
	g, err := source.Resolve(ctx, filepath.Join(dir, "git"))
	if err != nil {
		t.Fatal(err)
	}
	inspector := &source.Inspector{Git: g, MirrorRoot: filepath.Join(dir, "inspect")}
	c, err := inspector.Capture(ctx, repo, source.Selection{Untracked: []string{"notes/n.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) store.ArtifactRef {
		ref, err := e.cfg.Artifacts.Write(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		return store.ArtifactRef{Hash: ref.Hash, Size: ref.Size}
	}
	snap := store.Snapshot{ID: "snap", Project: "p", BaseCommit: c.Inspection.Identity.Head, Fingerprint: c.Inspection.Fingerprint, Staged: write(c.Staged), Unstaged: write(c.Unstaged)}
	for _, f := range c.Untracked {
		snap.Untracked = append(snap.Untracked, store.SnapshotFile{Path: f.Path, Mode: f.Mode, Artifact: write(f.Content)})
	}
	_, err = s.Do(ctx, "u", "bind", "bind", nil, func(tx *store.Tx) ([]byte, error) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO project_sources(project,canonical_path,common_dir,fingerprint,imported) VALUES('p',?,?,?,'now')", repo, filepath.Join(repo, ".git"), c.Inspection.Fingerprint); err != nil {
			return nil, err
		}
		if _, err := tx.RecordSnapshot(ctx, snap); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, "UPDATE tasks SET snapshot='snap' WHERE id='task'")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Workspaces = &source.Workspaces{Inspector: inspector, Root: filepath.Join(dir, "projects")}
	worktree = e.cfg.Workspaces.WorktreePath("p", "attempt")
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_DIR="+worktree)
	// Registered after every t.TempDir above, so it runs before their removal.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), preparationBound+e.StopBudget())
		defer cancel()
		if err := e.Stop(ctx); err != nil {
			t.Errorf("stop the executor: %v", err)
		}
	})
	return e, s, marker, repo, worktree
}

func sameSums(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("%d files before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("%s changed", path)
		}
	}
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return resolved
}

func TestTheEngineRunsInTheTasksWorktreeWithTheSnapshotCommitted(t *testing.T) {
	e, s, marker, repo, worktree := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	before := treeSums(t, repo)
	// The times go in the log, to size preparationBound by.
	start := time.Now()
	e.cfg.Workspaces.AfterSync = func(_ context.Context, cloned bool) {
		t.Logf("synced the clone (cloned %v) %v after the start", cloned, time.Since(start))
	}
	e.beforeCheck = func(string) { t.Logf("prepared the first workspace %v after the start", time.Since(start)) }
	e.LaunchAttempt("attempt")
	waitAttemptPrepared(t, s, "ended_ok")
	cwd := ""
	for _, line := range markerLines(t, marker) {
		if after, ok := strings.CutPrefix(line, "cwd:"); ok {
			cwd = after
		}
	}
	if cwd == "" || canonical(t, cwd) != canonical(t, worktree) {
		t.Fatalf("the engine ran in %q, not the worktree %q", cwd, worktree)
	}
	for _, p := range []string{"a.txt", "notes/n.bin"} {
		want, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(p)))
		if got, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(p))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: worktree %q, original %q, %v", p, got, want, err)
		}
	}
	if got := fixtureGit(t, worktree, "log", "-1", "--format=%s %an"); got != "Imported working changes Ada" {
		t.Fatalf("worktree HEAD = %q", got)
	}
	row, err := s.Workspace(context.Background(), "attempt")
	if err != nil || row.Path != worktree || row.Branch != "ada/task" || row.Snapshot != "snap" || row.Task != "task" || row.Project != "p" || row.Policy != "auto" || row.Executor != "local" || len(row.ManifestHash) != 64 {
		t.Fatalf("workspace row = %+v, %v", row, err)
	}
	// A second preparation fetches into the clone the first one made.
	start = time.Now()
	if _, err := e.cfg.Workspaces.Prepare(context.Background(), repo, source.Manifest{Project: "p", Task: "t2", Attempt: "a2", Snapshot: "snap", BaseCommit: row.BaseCommit, Executor: "local", Policy: "auto"}, source.Changes{}); err != nil {
		t.Fatalf("a second prepare: %v", err)
	}
	t.Logf("prepared the second workspace %v after its start", time.Since(start))
	sameSums(t, before, treeSums(t, repo))
}

func TestAMismatchedWorkspaceGoesToRecoveryRequiredAndIsNotRebuilt(t *testing.T) {
	for _, c := range []struct {
		name   string
		tamper func(t *testing.T, s *store.Store, manifest, worktree string)
		after  func(t *testing.T, manifest, worktree string)
	}{
		{"the manifest copy", func(t *testing.T, _ *store.Store, manifest, _ string) {
			data, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, bytes.Replace(data, []byte(`"auto"`), []byte(`"yolo"`), 1), 0o600); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, manifest, _ string) {
			if data, _ := os.ReadFile(manifest); !bytes.Contains(data, []byte(`"yolo"`)) {
				t.Fatal("the tampered manifest copy was rewritten")
			}
		}},
		{"the workspace row", func(t *testing.T, s *store.Store, _, _ string) {
			_, err := s.Do(context.Background(), "u", "tamper", "tamper", nil, func(tx *store.Tx) ([]byte, error) {
				_, err := tx.ExecContext(context.Background(), "UPDATE workspaces SET policy='yolo' WHERE attempt='attempt'")
				return []byte("ok"), err
			})
			if err != nil {
				t.Fatal(err)
			}
		}, func(*testing.T, string, string) {}},
		{"the whole record, rewritten consistently for another policy", func(t *testing.T, s *store.Store, manifest, _ string) {
			row, err := s.Workspace(context.Background(), "attempt")
			if err != nil {
				t.Fatal(err)
			}
			m := source.Manifest{Version: row.ManifestVersion, Project: row.Project, Task: row.Task, Attempt: row.Attempt, Snapshot: row.Snapshot,
				BaseCommit: row.BaseCommit, Branch: row.Branch, Path: row.Path, Executor: row.Executor, Policy: "yolo"}
			if err := os.WriteFile(manifest, m.Encode(), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = s.Do(context.Background(), "u", "rewrite", "rewrite", nil, func(tx *store.Tx) ([]byte, error) {
				_, err := tx.ExecContext(context.Background(), "UPDATE workspaces SET policy='yolo',manifest_hash=? WHERE attempt='attempt'", m.Hash())
				return []byte("ok"), err
			})
			if err != nil {
				t.Fatal(err)
			}
		}, func(*testing.T, string, string) {}},
		{"the task's branch, moved back to the base", func(t *testing.T, _ *store.Store, _, worktree string) {
			fixtureGit(t, worktree, "reset", "-q", "--soft", "HEAD~1")
		}, func(*testing.T, string, string) {}},
		{"the imported commit, replaced", func(t *testing.T, _ *store.Store, _, worktree string) {
			fixtureGit(t, worktree, "commit", "-q", "--amend", "-m", "replaced")
		}, func(*testing.T, string, string) {}},
		{"the worktree, deleted", func(t *testing.T, _ *store.Store, _, worktree string) {
			if err := os.RemoveAll(worktree); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, _, worktree string) {
			if _, err := os.Lstat(worktree); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the worktree was rebuilt: %v", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, s, marker, _, worktree := workspaceExecutor(t, "happy")
			manifest := filepath.Join(e.cfg.Workspaces.Root, "p", "repo.git", "worktrees", "attempt", "ada-manifest.json")
			e.beforeCheck = func(string) { c.tamper(t, s, manifest, worktree) }
			e.LaunchAttempt("attempt")
			waitAttemptPrepared(t, s, "recovery_required")
			task, err := s.Task(context.Background(), "u", "task")
			if err != nil || task.State != store.RecoveryRequired {
				t.Fatalf("task = %+v, %v", task, err)
			}
			events, err := s.Events(context.Background(), "u", "attempt", 0)
			if err != nil || len(events) != 1 || events[0].Kind != "recovery" || !bytes.Contains(events[0].Payload, []byte("workspace manifest mismatch")) {
				t.Fatalf("events = %+v, %v", events, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the engine started: %v", err)
			}
			c.after(t, manifest, worktree)
		})
	}
}

func TestACancelOrStopDuringPreparationFailsTheAttemptAndFreesTheProject(t *testing.T) {
	for _, c := range []struct {
		name, state string
		end         func(e *Executor)
	}{
		{"cancel", "ended_failed", func(e *Executor) { e.CancelTask("task") }},
		{"stop", "recovery_required", func(e *Executor) { e.StopWork() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, s, marker, repo, _ := workspaceExecutor(t, "happy")
			inside, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			e.cfg.Workspaces.AfterSync = func(ctx context.Context, _ bool) {
				once.Do(func() {
					close(inside)
					<-ctx.Done() // held until the preparation's context ends
					<-release
				})
			}
			e.LaunchAttempt("attempt")
			select {
			case <-inside:
			case <-time.After(preparationBound):
				t.Fatal("the preparation never synced")
			}
			c.end(e)
			close(release)
			waitAttemptPrepared(t, s, c.state)
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the engine started: %v", err)
			}
			events, err := s.Events(context.Background(), "u", "attempt", 0)
			if err != nil || len(events) == 0 || !bytes.Contains(events[len(events)-1].Payload, []byte("context canceled")) {
				t.Fatalf("events = %+v, %v", events, err)
			}
			// The project's lock was released: another preparation goes through.
			base := fixtureGit(t, repo, "rev-parse", "HEAD")
			done, finished := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(finished)
				_, err := e.cfg.Workspaces.Prepare(context.Background(), repo, source.Manifest{Project: "p", Task: "t2", Attempt: "a2", Snapshot: "s2", BaseCommit: base, Executor: "local", Policy: "auto"}, source.Changes{})
				done <- err
			}()
			// The executor's stop does not cover this preparation: a failed
			// test still waits for its git before the cleanup removes the directories.
			t.Cleanup(func() {
				select {
				case <-finished:
				case <-time.After(preparationBound):
					t.Error("the later prepare is still running")
				}
			})
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("a later prepare: %v", err)
				}
			case <-time.After(preparationBound):
				t.Fatal("the project's lock is still held")
			}
		})
	}
}

// snapshotBlob is the stored file of one of the bound snapshot's artifacts.
func snapshotBlob(t *testing.T, e *Executor, s *store.Store, pick func(store.Snapshot) store.ArtifactRef) string {
	t.Helper()
	src, err := s.AttemptSource(context.Background(), "attempt")
	if err != nil || src.Snapshot == nil {
		t.Fatalf("attempt source = %+v, %v", src, err)
	}
	hash := pick(*src.Snapshot).Hash
	return filepath.Join(e.cfg.StateDir, "artifacts", hash[:2], hash)
}

func flipLastByte(t *testing.T, blob string) {
	t.Helper()
	data, err := os.ReadFile(blob)
	if err != nil || len(data) == 0 {
		t.Fatalf("blob %s: %d bytes, %v", blob, len(data), err)
	}
	data[len(data)-1] ^= 1
	if err := os.Chmod(blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// refusedBeforeTheEngine checks the attempt went to recovery with one
// recovery event holding each of want, and the engine never started.
func refusedBeforeTheEngine(t *testing.T, s *store.Store, marker string, want ...string) {
	t.Helper()
	waitAttemptPrepared(t, s, "recovery_required")
	if task, err := s.Task(context.Background(), "u", "task"); err != nil || task.State != store.RecoveryRequired {
		t.Fatalf("task = %+v, %v", task, err)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "recovery" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	for _, w := range want {
		if !bytes.Contains(events[0].Payload, []byte(w)) {
			t.Errorf("recovery event %s does not name %q", events[0].Payload, w)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the engine started: %v", err)
	}
}

func TestACorruptedSnapshotArtifactRefusesTheFirstLaunchBeforeTheWorktreeIsWritten(t *testing.T) {
	for _, c := range []struct {
		name    string
		pick    func(store.Snapshot) store.ArtifactRef
		corrupt func(t *testing.T, blob string)
	}{
		{"the unstaged patch, a byte flipped", func(s store.Snapshot) store.ArtifactRef { return s.Unstaged }, flipLastByte},
		{"an untracked file, a byte flipped", func(s store.Snapshot) store.ArtifactRef { return s.Untracked[0].Artifact }, flipLastByte},
		{"the staged patch, removed", func(s store.Snapshot) store.ArtifactRef { return s.Staged }, func(t *testing.T, blob string) {
			if err := os.Remove(blob); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, s, marker, _, worktree := workspaceExecutor(t, "happy")
			blob := snapshotBlob(t, e, s, c.pick)
			c.corrupt(t, blob)
			e.LaunchAttempt("attempt")
			refusedBeforeTheEngine(t, s, marker, "snapshot artifact mismatch", filepath.Base(blob))
			if _, err := os.Lstat(worktree); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the worktree was written: %v", err)
			}
			if _, err := s.Workspace(context.Background(), "attempt"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a workspace was recorded: %v", err)
			}
		})
	}
}

func TestASnapshotArtifactCorruptedAfterPreparationRefusesTheLaunch(t *testing.T) {
	e, s, marker, _, worktree := workspaceExecutor(t, "happy")
	blob := snapshotBlob(t, e, s, func(s store.Snapshot) store.ArtifactRef { return s.Untracked[0].Artifact })
	e.beforeCheck = func(string) { flipLastByte(t, blob) }
	e.LaunchAttempt("attempt")
	refusedBeforeTheEngine(t, s, marker, "snapshot artifact mismatch", filepath.Base(blob))
	// The prepared worktree is left as it was, neither removed nor rebuilt.
	if got := fixtureGit(t, worktree, "log", "-1", "--format=%s"); got != "Imported working changes" {
		t.Fatalf("worktree HEAD = %q", got)
	}
}

// rewriteManifest changes the workspace row and the manifest copy together,
// so they agree with each other and the row's hash.
func rewriteManifest(t *testing.T, s *store.Store, manifest string, change func(*source.Manifest)) {
	t.Helper()
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m source.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	change(&m)
	if err := os.WriteFile(manifest, m.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(context.Background(), "u", "rewrite", "rewrite", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), "UPDATE workspaces SET manifest_version=?,task_revision=?,ownership_generation=?,manifest_hash=? WHERE attempt='attempt'",
			m.Version, m.TaskRevision, m.Generation, m.Hash())
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAStaleManifestRefusesTheLaunch(t *testing.T) {
	update := func(t *testing.T, s *store.Store, query string) {
		t.Helper()
		_, err := s.Do(context.Background(), "u", query, query, nil, func(tx *store.Tx) ([]byte, error) {
			_, err := tx.ExecContext(context.Background(), query)
			return []byte("ok"), err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, field string
		stale       func(t *testing.T, s *store.Store, manifest string)
	}{
		{"the task revision moved", "task revision", func(t *testing.T, s *store.Store, _ string) {
			update(t, s, "UPDATE tasks SET revision=revision+1 WHERE id='task'")
		}},
		{"the task bound to another snapshot", "snapshot", func(t *testing.T, s *store.Store, _ string) {
			update(t, s, "INSERT INTO source_snapshots(id,project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created) SELECT 'snap2',project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created FROM source_snapshots WHERE id='snap'")
			update(t, s, "UPDATE tasks SET snapshot='snap2' WHERE id='task'")
		}},
		{"an older ownership generation", "ownership generation", func(t *testing.T, s *store.Store, manifest string) {
			rewriteManifest(t, s, manifest, func(m *source.Manifest) { m.Generation-- })
		}},
		{"an older manifest version", "version", func(t *testing.T, s *store.Store, manifest string) {
			rewriteManifest(t, s, manifest, func(m *source.Manifest) { m.Version = 1 })
		}},
		// A row written before version 2 bound no revision or generation; it reads -1.
		{"a version-1 row", "version, task revision, ownership generation", func(t *testing.T, s *store.Store, _ string) {
			update(t, s, "UPDATE workspaces SET manifest_version=1,task_revision=NULL,ownership_generation=NULL WHERE attempt='attempt'")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, s, marker, _, _ := workspaceExecutor(t, "happy")
			manifest := filepath.Join(e.cfg.Workspaces.Root, "p", "repo.git", "worktrees", "attempt", "ada-manifest.json")
			e.beforeCheck = func(string) { c.stale(t, s, manifest) }
			e.LaunchAttempt("attempt")
			refusedBeforeTheEngine(t, s, marker, "workspace manifest mismatch", c.field)
		})
	}
}

func TestTheManifestBindsTheTaskRevisionAndOwnershipGeneration(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	e.LaunchAttempt("attempt")
	waitAttemptPrepared(t, s, "ended_ok")
	row, err := s.Workspace(context.Background(), "attempt")
	if err != nil || row.ManifestVersion != source.ManifestVersion || source.ManifestVersion != 2 || row.TaskRevision != 2 || row.Generation != e.cfg.Generation {
		t.Fatalf("workspace row = %+v, %v", row, err)
	}
}
