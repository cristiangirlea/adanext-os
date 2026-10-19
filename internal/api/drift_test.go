package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type driftResult struct {
	Snapshot   string   `json:"snapshot"`
	Drift      bool     `json:"drift"`
	HeadMoved  bool     `json:"head_moved"`
	NewCommits []any    `json:"new_commits"`
	Changed    []string `json:"changed"`
	Untracked  []string `json:"untracked"`
	Blockers   []any    `json:"blockers"`
}

func TestDriftComparesTheOriginalWithTheNewestSnapshotForMembersAndStoresNothing(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	path := "/v1/projects/" + project.ID + "/drift"
	if status, data := p.call(t, "alice", "GET", path, "", nil); status != 409 || !strings.Contains(string(data), `"error":"no_snapshot"`) {
		t.Fatalf("no snapshot yet = %d %s", status, data)
	}
	p.snapshotOf(t, "snap-1", project.ID)
	writeFile(t, filepath.Join(dir, "a.txt"), "edited before the second snapshot\n")
	newest := p.snapshotOf(t, "snap-2", project.ID)

	before, rows := treeSums(t, dir), p.count(t, "SELECT (SELECT count(*) FROM source_snapshots)+(SELECT count(*) FROM artifacts)+(SELECT count(*) FROM idempotency)")
	status, data := p.call(t, "alice", "GET", path, "", nil)
	got := decodeJSON[driftResult](t, data)
	if status != 200 || got.Drift || got.Snapshot != newest || got.NewCommits == nil || got.Changed == nil || got.Untracked == nil || got.Blockers == nil ||
		len(got.NewCommits)+len(got.Changed)+len(got.Untracked)+len(got.Blockers) != 0 {
		t.Fatalf("unchanged since the newest snapshot = %d %s", status, data)
	}
	sameSums := func() {
		t.Helper()
		after := treeSums(t, dir)
		if len(after) != len(before) {
			t.Fatalf("%d files before, %d after", len(before), len(after))
		}
		for file, sum := range before {
			if after[file] != sum {
				t.Fatalf("%s changed", file)
			}
		}
	}
	sameSums()

	writeFile(t, filepath.Join(dir, "a.txt"), "edited after it\n")
	writeFile(t, filepath.Join(dir, "fresh.txt"), "new\n")
	before = treeSums(t, dir)
	status, data = p.call(t, "alice", "GET", path, "", nil)
	got = decodeJSON[driftResult](t, data)
	if status != 200 || !got.Drift || got.HeadMoved || !slices.Equal(got.Changed, []string{"a.txt"}) || !slices.Equal(got.Untracked, []string{"fresh.txt"}) {
		t.Fatalf("after an edit and a new file = %d %s", status, data)
	}
	sameSums()
	if after := p.count(t, "SELECT (SELECT count(*) FROM source_snapshots)+(SELECT count(*) FROM artifacts)+(SELECT count(*) FROM idempotency)"); after != rows {
		t.Fatalf("drift stored something: %d rows, then %d", rows, after)
	}

	if status, data := p.call(t, "bob", "GET", path, "", nil); status != 404 {
		t.Fatalf("a non-member = %d %s", status, data)
	}
	if status, data := p.call(t, "alice", "GET", "/v1/projects/missing/drift", "", nil); status != 404 {
		t.Fatalf("an unknown project = %d %s", status, data)
	}
	if status, data := p.call(t, "alice", "GET", "/v1/projects/a/drift", "", nil); status != 409 || !strings.Contains(string(data), `"error":"no_source"`) {
		t.Fatalf("a project without a source = %d %s", status, data)
	}
}

func TestDriftNeedsGitAndASnapshotThatRecordedItsPaths(t *testing.T) {
	p := newProjectsFixture(t)
	project := p.importRepo(t, "alice", "import-1", newRepo(t))
	p.snapshotOf(t, "snap-1", project.ID)
	path := "/v1/projects/" + project.ID + "/drift"

	server := httptest.NewServer(New(p.s, p.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }, Execution{Inspector: p.inspector, Capturer: p.inspector}))
	q := &projectsFixture{fixture: p.fixture, url: server.URL}
	status, data := q.call(t, "alice", "GET", path, "", nil)
	server.Close()
	if status != 503 || !strings.Contains(string(data), `"error":"git_unavailable"`) {
		t.Fatalf("without git = %d %s", status, data)
	}

	// A snapshot taken before migration 0007 recorded no branch or per-path entries.
	_, err := p.s.Do(context.Background(), "alice", "legacy", "legacy", nil, func(tx *store.Tx) ([]byte, error) {
		if _, err := tx.ExecContext(context.Background(), "DELETE FROM snapshot_paths"); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(context.Background(), "UPDATE source_snapshots SET branch=NULL")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, data := p.call(t, "alice", "GET", path, "", nil); status != 409 || !strings.Contains(string(data), `"error":"snapshot_outdated"`) {
		t.Fatalf("a snapshot from before drift = %d %s", status, data)
	}
}

func TestDriftOfAnotherRepositoryAtTheSourcePathIsRefused(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	p.snapshotOf(t, "snap-1", project.ID)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "--separate-git-dir", filepath.Join(canonicalTemp(t), "elsewhere.git"))
	path := "/v1/projects/" + project.ID + "/drift"
	if status, data := p.call(t, "alice", "GET", path, "", nil); status != 409 || !strings.Contains(string(data), `"error":"source_changed"`) {
		t.Fatalf("another repository = %d %s", status, data)
	}
	// With no repository left at all, drift reports the blocker.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	status, data := p.call(t, "alice", "GET", path, "", nil)
	if status != 200 || !strings.Contains(string(data), `"code":"not_git_repository"`) || !strings.Contains(string(data), `"head_moved":false`) {
		t.Fatalf("the original deleted = %d %s", status, data)
	}
}

// failingDrifter answers every drift with an error.
type failingDrifter struct{}

func (failingDrifter) Drift(context.Context, string, source.Baseline) (source.Drift, error) {
	return source.Drift{}, errors.New("git failed")
}

func TestADriftThatFailsAnswersAnInternalError(t *testing.T) {
	p := newProjectsFixture(t, func(e *Execution) { e.Drifter = failingDrifter{} })
	project := p.importRepo(t, "alice", "import-1", newRepo(t))
	p.snapshotOf(t, "snap-1", project.ID)
	if status, data := p.call(t, "alice", "GET", "/v1/projects/"+project.ID+"/drift", "", nil); status != 500 || strings.Contains(string(data), "git failed") {
		t.Fatalf("a failed drift = %d %s", status, data)
	}
}
