package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// runGit runs the system git in dir to build a fixture, with the machine's
// own system and global config left out.
func runGit(t *testing.T, dir string, args ...string) string {
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

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// newRepo is a repository with one commit, then staged, unstaged and
// untracked work, a credential-looking file and an ignored one.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := canonicalTemp(t)
	runGit(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), "build/\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "first")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\nstaged\n")
	runGit(t, dir, "add", "a.txt")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\nstaged\nunstaged\n")
	writeFile(t, filepath.Join(dir, "notes", "n.bin"), "\x00\x01note\r\n")
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=x\n")
	writeFile(t, filepath.Join(dir, "build", "out.bin"), "built\n")
	return dir
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

type countingInspector struct {
	in       *source.Inspector
	inspects atomic.Int32
	captures atomic.Int32
}

func (c *countingInspector) Inspect(ctx context.Context, path string) (source.Inspection, error) {
	c.inspects.Add(1)
	return c.in.Inspect(ctx, path)
}

func (c *countingInspector) Capture(ctx context.Context, path string, sel source.Selection) (source.Capture, error) {
	c.captures.Add(1)
	return c.in.Capture(ctx, path, sel)
}

type projectsFixture struct {
	*fixture
	url       string
	inspector *countingInspector
	artifacts *artifact.Store
}

// newProjectsFixture serves the API with the real inspector and artifact
// store; the X-Test-User header names the caller.
func newProjectsFixture(t *testing.T, change ...func(*Execution)) *projectsFixture {
	t.Helper()
	f := newFixture(t)
	g, err := source.Resolve(context.Background(), canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	inspector := &countingInspector{in: &source.Inspector{Git: g, MirrorRoot: canonicalTemp(t)}}
	artifacts, err := artifact.Open(canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	execution := Execution{Inspector: inspector, Capturer: inspector, Artifacts: artifacts, Writer: artifacts}
	for _, c := range change {
		c(&execution)
	}
	server := httptest.NewServer(New(f.s, f.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }, execution))
	t.Cleanup(server.Close)
	return &projectsFixture{fixture: f, url: server.URL, inspector: inspector, artifacts: artifacts}
}

var errRollback = errors.New("rolled back")

// count reads one number in a transaction that then rolls back, storing nothing.
func (p *projectsFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.s.Do(context.Background(), "alice", "count-"+id, "count", nil, func(tx *store.Tx) ([]byte, error) {
		if err := tx.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
			return nil, err
		}
		return nil, errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	return n
}

func (p *projectsFixture) call(t *testing.T, user, method, path, key string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, p.url+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", user)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func decodeJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}

type projectResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Source struct {
		Path        string `json:"path"`
		CommonDir   string `json:"common_dir"`
		Fingerprint string `json:"fingerprint"`
		Imported    string `json:"imported"`
	} `json:"source"`
}

func (p *projectsFixture) importRepo(t *testing.T, user, key, dir string) projectResult {
	t.Helper()
	status, data := p.call(t, user, "POST", "/v1/projects", key, map[string]string{"path": dir})
	if status != 201 {
		t.Fatalf("import = %d %s", status, data)
	}
	return decodeJSON[projectResult](t, data)
}

func TestImportCreatesTheProjectWithItsCreatorAsOwnerAndReplaysByKey(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	before := treeSums(t, dir)
	status, first := p.call(t, "alice", "POST", "/v1/projects", "import-1", map[string]string{"path": dir, "name": "Mine"})
	if status != 201 {
		t.Fatalf("import = %d %s", status, first)
	}
	got := decodeJSON[projectResult](t, first)
	if got.ID == "" || got.Name != "Mine" || got.Role != "owner" || got.Source.Path != dir || got.Source.CommonDir != filepath.Join(dir, ".git") || got.Source.Fingerprint == "" || got.Source.Imported == "" {
		t.Fatalf("project = %+v", got)
	}
	src, err := p.s.ProjectSource(context.Background(), "alice", got.ID)
	if err != nil || src.Path != dir || src.Fingerprint != got.Source.Fingerprint {
		t.Fatalf("source = %+v, %v", src, err)
	}
	if _, err := p.s.ProjectSource(context.Background(), "bob", got.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob sees the project: %v", err)
	}
	inspects := p.inspector.inspects.Load()
	status, again := p.call(t, "alice", "POST", "/v1/projects", "import-1", map[string]string{"path": dir, "name": "Mine"})
	if status != 201 || !bytes.Equal(first, again) || p.inspector.inspects.Load() != inspects {
		t.Fatalf("replay = %d %s, inspections %d -> %d", status, again, inspects, p.inspector.inspects.Load())
	}
	if status, data := p.call(t, "alice", "POST", "/v1/projects", "import-1", map[string]string{"path": dir}); status != 422 || !strings.Contains(string(data), `"idempotency_conflict"`) {
		t.Fatalf("same key, other body = %d %s", status, data)
	}
	if status, data := p.call(t, "alice", "POST", "/v1/projects", "", map[string]string{"path": dir}); status != 400 {
		t.Fatalf("no key = %d %s", status, data)
	}
	other := newRepo(t)
	if named := p.importRepo(t, "alice", "import-2", other); named.Name != filepath.Base(other) {
		t.Fatalf("default name = %q", named.Name)
	}
	after := treeSums(t, dir)
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("%s changed", path)
		}
	}
	if len(after) != len(before) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}
}

func TestImportRefusesARepositoryWithBlockersAndCreatesNothing(t *testing.T) {
	p := newProjectsFixture(t)
	merging := newRepo(t)
	writeFile(t, filepath.Join(merging, ".git", "MERGE_HEAD"), runGit(t, merging, "rev-parse", "HEAD")+"\n")
	for dir, code := range map[string]string{canonicalTemp(t): source.BlockNotRepository, merging: source.BlockMerge} {
		status, data := p.call(t, "alice", "POST", "/v1/projects", "blocked-"+code, map[string]string{"path": dir})
		got := decodeJSON[struct {
			Error    string           `json:"error"`
			Blockers []source.Blocker `json:"blockers"`
		}](t, data)
		if status != 409 || got.Error != "import_blocked" || len(got.Blockers) == 0 || got.Blockers[0].Code != code {
			t.Errorf("%s: %d %s", code, status, data)
		}
	}
	if n := p.count(t, "SELECT count(*) FROM project_sources"); n != 0 {
		t.Fatalf("%d sources", n)
	}
}

func TestImportRefusesABadRequestBeforeInspecting(t *testing.T) {
	p := newProjectsFixture(t)
	for i, body := range []string{`{"path":"relative"}`, `{}`, `{"path":"\\\\host\\share\\repo"}`, `{"path":"/x","extra":1}`, `{"path":"/x","name":"a\u0000b"}`, `{"path":"/x","name":"` + strings.Repeat("n", 201) + `"}`, `not json`} {
		if status, data := p.call(t, "alice", "POST", "/v1/projects", fmt.Sprintf("bad-%d", i), body); status != 400 {
			t.Errorf("%s: %d %s", body, status, data)
		}
	}
	if p.inspector.inspects.Load() != 0 {
		t.Fatal("a bad request was inspected")
	}
}

func TestASecondImportNamesTheFirstProjectAndItsTasksToAMemberOnly(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	first := p.importRepo(t, "alice", "import-1", dir)
	status, data := p.call(t, "alice", "POST", "/v1/tasks", "task-1", map[string]string{"project": first.ID, "objective": "fix it", "criteria": "done"})
	if status != 201 {
		t.Fatalf("task = %d %s", status, data)
	}
	task := decodeJSON[struct{ ID string }](t, data)
	type already struct {
		Error   string  `json:"error"`
		Project *string `json:"project"`
		Tasks   []struct {
			ID        string `json:"id"`
			Objective string `json:"objective"`
			State     string `json:"state"`
		} `json:"tasks"`
	}
	status, data = p.call(t, "alice", "POST", "/v1/projects", "import-2", map[string]string{"path": dir})
	got := decodeJSON[already](t, data)
	if status != 409 || got.Error != "already_imported" || got.Project == nil || *got.Project != first.ID || len(got.Tasks) != 1 || got.Tasks[0].ID != task.ID || got.Tasks[0].Objective != "fix it" || got.Tasks[0].State != "draft" {
		t.Fatalf("member = %d %s", status, data)
	}
	status, data = p.call(t, "bob", "POST", "/v1/projects", "import-3", map[string]string{"path": dir})
	got = decodeJSON[already](t, data)
	if status != 409 || got.Error != "already_imported" || got.Project != nil || got.Tasks != nil || strings.Contains(string(data), first.ID) {
		t.Fatalf("non-member = %d %s", status, data)
	}
}

type snapshotResult struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	BaseCommit  string `json:"base_commit"`
	Fingerprint string `json:"fingerprint"`
	Staged      struct {
		Hash string `json:"hash"`
		Size int64  `json:"size"`
	} `json:"staged_patch"`
	Unstaged struct {
		Hash string `json:"hash"`
		Size int64  `json:"size"`
	} `json:"unstaged_patch"`
	Untracked []struct {
		Path     string `json:"path"`
		Mode     string `json:"mode"`
		Artifact struct {
			Hash string `json:"hash"`
			Size int64  `json:"size"`
		} `json:"artifact"`
	} `json:"untracked"`
	CreatedBy string `json:"created_by"`
	Created   string `json:"created"`
}

func (p *projectsFixture) content(t *testing.T, hash string) []byte {
	t.Helper()
	r, err := p.artifacts.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSnapshotStoresItsPatchesAndSelectedFilesForMembers(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	before := treeSums(t, dir)
	path := "/v1/projects/" + project.ID + "/snapshots"
	body := map[string][]string{"untracked": {"notes/n.bin", ".env", "build/out.bin"}, "include_sensitive": {".env", "build/out.bin"}}
	status, data := p.call(t, "alice", "POST", path, "snap-1", body)
	if status != 201 {
		t.Fatalf("snapshot = %d %s", status, data)
	}
	got := decodeJSON[snapshotResult](t, data)
	head := runGit(t, dir, "rev-parse", "HEAD")
	if got.ID == "" || got.Project != project.ID || got.BaseCommit != head || got.Fingerprint != project.Source.Fingerprint || got.CreatedBy != "alice" || len(got.Untracked) != 3 {
		t.Fatalf("snapshot = %s", data)
	}
	if staged := p.content(t, got.Staged.Hash); !bytes.Contains(staged, []byte("+staged\n")) || bytes.Contains(staged, []byte("+unstaged")) || int64(len(staged)) != got.Staged.Size {
		t.Errorf("staged patch = %q", staged)
	}
	if unstaged := p.content(t, got.Unstaged.Hash); !bytes.Contains(unstaged, []byte("+unstaged\n")) || int64(len(unstaged)) != got.Unstaged.Size {
		t.Errorf("unstaged patch = %q", unstaged)
	}
	for i, want := range []struct{ path, content string }{{"notes/n.bin", "\x00\x01note\r\n"}, {".env", "TOKEN=x\n"}, {"build/out.bin", "built\n"}} {
		f := got.Untracked[i]
		if f.Path != want.path || f.Mode != "100644" || string(p.content(t, f.Artifact.Hash)) != want.content {
			t.Errorf("untracked %d = %+v", i, f)
		}
	}
	after := treeSums(t, dir)
	for file, sum := range before {
		if after[file] != sum {
			t.Errorf("%s changed", file)
		}
	}
	if len(after) != len(before) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}

	captures := p.inspector.captures.Load()
	if status, again := p.call(t, "alice", "POST", path, "snap-1", body); status != 201 || !bytes.Equal(again, data) || p.inspector.captures.Load() != captures {
		t.Fatalf("replay = %d %s, captures %d -> %d", status, again, captures, p.inspector.captures.Load())
	}
	if status, read := p.call(t, "alice", "GET", path+"/"+got.ID, "", nil); status != 200 || !bytes.Equal(bytes.TrimSpace(read), bytes.TrimSpace(data)) {
		t.Fatalf("read = %d %s", status, read)
	}
	if status, read := p.call(t, "bob", "GET", path+"/"+got.ID, "", nil); status != 404 || !strings.Contains(string(read), `"not_found"`) {
		t.Fatalf("non-member read = %d %s", status, read)
	}
	if status, read := p.call(t, "alice", "GET", path+"/missing", "", nil); status != 404 {
		t.Fatalf("unknown read = %d %s", status, read)
	}
	if status, read := p.call(t, "bob", "POST", path, "snap-bob", body); status != 404 || p.inspector.captures.Load() != captures {
		t.Fatalf("non-member snapshot = %d %s, captures %d", status, read, p.inspector.captures.Load())
	}
	if err := p.s.RemoveMember(context.Background(), project.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if status, read := p.call(t, "alice", "POST", path, "snap-1", body); status != 404 {
		t.Fatalf("removed member replay = %d %s", status, read)
	}
}

func TestSnapshotRefusesBadSelectionsWithTheirCodeAndStoresNothing(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	path := "/v1/projects/" + project.ID + "/snapshots"
	for i, c := range []struct {
		untracked []string
		include   []string
		status    int
		code      string
	}{
		{[]string{"../outside"}, nil, 422, "invalid_path"},
		{[]string{"a.txt"}, nil, 422, "invalid_path"},
		{[]string{"notes/n.bin:stream"}, nil, 422, "invalid_path"},
		{[]string{".env"}, nil, 422, "sensitive_path"},
		{[]string{"build/out.bin"}, nil, 422, "sensitive_path"},
		{[]string{"notes/n.bin", "notes/n.bin"}, nil, 400, "bad_request"},
		{[]string{"notes/n.bin"}, []string{".env"}, 400, "bad_request"},
		{[]string{"a\x00b"}, nil, 400, "bad_request"},
	} {
		status, data := p.call(t, "alice", "POST", path, fmt.Sprintf("bad-%d", i), map[string][]string{"untracked": c.untracked, "include_sensitive": c.include})
		if status != c.status || !strings.Contains(string(data), `"error":"`+c.code+`"`) {
			t.Errorf("%v %v: %d %s", c.untracked, c.include, status, data)
		}
		if c.status == 422 && !strings.Contains(string(data), `"path":`) {
			t.Errorf("%v: the refusal does not name the path: %s", c.untracked, data)
		}
	}
	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("f%d", i)
	}
	if status, data := p.call(t, "alice", "POST", path, "too-many", map[string][]string{"untracked": tooMany}); status != 400 {
		t.Errorf("too many = %d %s", status, data)
	}
	if n := p.count(t, "SELECT (SELECT count(*) FROM source_snapshots)+(SELECT count(*) FROM artifacts)"); n != 0 {
		t.Fatalf("stored %d snapshots and artifacts", n)
	}
}

type fakeCapturer struct{ err error }

func (f fakeCapturer) Capture(context.Context, string, source.Selection) (source.Capture, error) {
	return source.Capture{}, f.err
}

type countingWriter struct {
	sync.Mutex
	writes atomic.Int32
}

func (c *countingWriter) Write(context.Context, io.Reader, int64) (artifact.Ref, error) {
	c.writes.Add(1)
	return artifact.Ref{}, errors.New("unexpected write")
}

func (c *countingWriter) Remove(string) error { return errors.New("unexpected remove") }

// removingWriter records the blobs the handler removes.
type removingWriter struct {
	*artifact.Store
	mu      sync.Mutex
	removed []string
}

func (w *removingWriter) Remove(hash string) error {
	w.mu.Lock()
	w.removed = append(w.removed, hash)
	w.mu.Unlock()
	return w.Store.Remove(hash)
}

// hookedCapturer runs after once each capture returns.
type hookedCapturer struct {
	in    *countingInspector
	after func()
}

func (c hookedCapturer) Capture(ctx context.Context, path string, sel source.Selection) (source.Capture, error) {
	captured, err := c.in.Capture(ctx, path, sel)
	if c.after != nil {
		c.after()
	}
	return captured, err
}

func TestAFailedSnapshotRemovesTheBlobsOnlyItWrote(t *testing.T) {
	writer := &removingWriter{}
	capturer := &hookedCapturer{}
	p := newProjectsFixture(t, func(e *Execution) { e.Capturer, e.Writer = capturer, writer })
	writer.Store, capturer.in = p.artifacts, p.inspector
	project := p.importRepo(t, "alice", "import-1", newRepo(t))
	path := "/v1/projects/" + project.ID + "/snapshots"
	status, data := p.call(t, "alice", "POST", path, "snap-1", map[string][]string{})
	if status != 201 {
		t.Fatalf("first snapshot = %d %s", status, data)
	}
	first := decodeJSON[snapshotResult](t, data)
	// The member is removed during the second capture, so its record fails
	// after the patches (committed by the first) and the new file are written.
	capturer.after = func() {
		if err := p.s.RemoveMember(context.Background(), project.ID, "alice"); err != nil {
			t.Error(err)
		}
	}
	if status, data := p.call(t, "alice", "POST", path, "snap-2", map[string][]string{"untracked": {"notes/n.bin"}}); status != 404 {
		t.Fatalf("second snapshot = %d %s", status, data)
	}
	sum := sha256.Sum256([]byte("\x00\x01note\r\n"))
	if want := []string{hex.EncodeToString(sum[:])}; !slices.Equal(writer.removed, want) {
		t.Fatalf("removed %v, want %v", writer.removed, want)
	}
	if _, err := p.artifacts.Open(writer.removed[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed blob opens: %v", err)
	}
	p.content(t, first.Staged.Hash)
	p.content(t, first.Unstaged.Hash)
}

func TestSnapshotCaptureRefusalsAnswerTheirCodeAndStoreNothing(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{source.ErrSourceChanged, 409, "source_changed"},
		{&source.BlockedError{Blockers: []source.Blocker{{Code: source.BlockMerge, Paths: []string{"MERGE_HEAD"}, Count: 1}}}, 409, "import_blocked"},
		{&source.PathError{Code: source.PathInvalid, Path: "d/f.txt", Reason: "a link or junction on its path"}, 422, "invalid_path"},
		{fmt.Errorf("%w: big.bin", source.ErrTooLarge), 413, "capture_too_large"},
		{errors.New("git diff failed"), 500, "internal"},
	} {
		writer := &countingWriter{}
		p := newProjectsFixture(t, func(e *Execution) { e.Capturer, e.Writer = fakeCapturer{c.err}, writer })
		project := p.importRepo(t, "alice", "import-1", newRepo(t))
		status, data := p.call(t, "alice", "POST", "/v1/projects/"+project.ID+"/snapshots", "snap", map[string][]string{"untracked": {"d/f.txt"}})
		if status != c.status || !strings.Contains(string(data), `"error":"`+c.code+`"`) {
			t.Errorf("%v: %d %s", c.err, status, data)
		}
		if n := p.count(t, "SELECT (SELECT count(*) FROM source_snapshots)+(SELECT count(*) FROM idempotency WHERE key='snap')"); n != 0 || writer.writes.Load() != 0 {
			t.Errorf("%v: stored %d snapshots and keys, wrote %d artifacts", c.err, n, writer.writes.Load())
		}
	}
}

func TestSnapshotNeedsAnImportedSourceAndAvailableGitAndArtifacts(t *testing.T) {
	p := newProjectsFixture(t)
	if status, data := p.call(t, "alice", "POST", "/v1/projects/a/snapshots", "snap", map[string][]string{}); status != 409 || !strings.Contains(string(data), `"no_source"`) {
		t.Fatalf("project without a source = %d %s", status, data)
	}
	if status, data := p.call(t, "alice", "POST", "/v1/projects/missing/snapshots", "snap-2", map[string][]string{}); status != 404 {
		t.Fatalf("unknown project = %d %s", status, data)
	}
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	for name, change := range map[string]func(*Execution){
		"git_unavailable":       func(e *Execution) { e.Capturer = nil },
		"artifacts_unavailable": func(e *Execution) { e.Writer = nil },
	} {
		e := Execution{Inspector: p.inspector, Capturer: p.inspector, Writer: p.artifacts}
		change(&e)
		server := httptest.NewServer(New(p.s, p.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }, e))
		q := &projectsFixture{fixture: p.fixture, url: server.URL}
		status, data := q.call(t, "alice", "POST", "/v1/projects/"+project.ID+"/snapshots", "snap-"+name, map[string][]string{})
		server.Close()
		if status != 503 || !strings.Contains(string(data), `"`+name+`"`) {
			t.Errorf("%s: %d %s", name, status, data)
		}
	}
}

func TestSnapshotOfAnotherRepositoryAtTheSourcePathIsRefused(t *testing.T) {
	p := newProjectsFixture(t)
	dir := newRepo(t)
	project := p.importRepo(t, "alice", "import-1", dir)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "--separate-git-dir", filepath.Join(canonicalTemp(t), "elsewhere.git"))
	status, data := p.call(t, "alice", "POST", "/v1/projects/"+project.ID+"/snapshots", "snap", map[string][]string{})
	if status != 409 || !strings.Contains(string(data), `"source_changed"`) {
		t.Fatalf("snapshot = %d %s", status, data)
	}
}

// heldCapturer holds its first capture until released; each capture is refused.
type heldCapturer struct {
	calls            atomic.Int32
	entered, release chan struct{}
}

func (c *heldCapturer) Capture(context.Context, string, source.Selection) (source.Capture, error) {
	if c.calls.Add(1) == 1 {
		close(c.entered)
		<-c.release
	}
	return source.Capture{}, source.ErrSourceChanged
}

func TestASecondSnapshotWhileOneIsCapturingIsBusy(t *testing.T) {
	held := &heldCapturer{entered: make(chan struct{}), release: make(chan struct{})}
	p := newProjectsFixture(t, func(e *Execution) { e.Capturer = held })
	project := p.importRepo(t, "alice", "import-1", newRepo(t))
	path := "/v1/projects/" + project.ID + "/snapshots"
	first := make(chan int, 1)
	go func() {
		status, _ := p.call(t, "alice", "POST", path, "snap-1", map[string][]string{})
		first <- status
	}()
	<-held.entered
	if status, data := p.call(t, "alice", "POST", path, "snap-2", map[string][]string{}); status != 429 || !strings.Contains(string(data), `"error":"capture_busy"`) {
		t.Errorf("a snapshot while another captures = %d %s", status, data)
	}
	close(held.release)
	if status := <-first; status != 409 {
		t.Fatalf("the first snapshot = %d", status)
	}
	if status, data := p.call(t, "alice", "POST", path, "snap-3", map[string][]string{}); status != 409 || !strings.Contains(string(data), `"source_changed"`) {
		t.Fatalf("a snapshot after the first ended = %d %s", status, data)
	}
}
