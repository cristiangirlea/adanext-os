package source

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newWorkspaces(t *testing.T) *Workspaces {
	t.Helper()
	return &Workspaces{Inspector: newInspector(t), Root: tempDir(t)}
}

func changesOf(c Capture) Changes {
	return Changes{Staged: c.Staged, Unstaged: c.Unstaged, Untracked: c.Untracked}
}

// prepareCapture prepares attempt a<id> of task t<id> in project p from c.
func prepareCapture(w *Workspaces, dir string, c Capture, id string) (Manifest, error) {
	m := Manifest{Project: "p", Task: "t" + id, Attempt: "a" + id, Snapshot: "s" + id, BaseCommit: c.Inspection.Identity.Head, Executor: "local", Policy: "auto"}
	return w.Prepare(context.Background(), dir, m, changesOf(c))
}

func mustCapture(t *testing.T, in *Inspector, dir string, untracked ...string) Capture {
	t.Helper()
	c, err := capture(in, dir, untracked)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return c
}

func mustPrepare(t *testing.T, w *Workspaces, dir string, c Capture, id string) Manifest {
	t.Helper()
	m, err := prepareCapture(w, dir, c, id)
	if err != nil {
		t.Fatalf("prepare %s: %v", id, err)
	}
	return m
}

// files lists the regular files under dir.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestTheCloneSharesNoFilesOrObjectStorageWithTheOriginal(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "one\n", "b.bin": "\x00\x01"})
	git(t, dir, "gc", "-q") // a pack, as well as loose objects below
	write(t, filepath.Join(dir, "c.txt"), "loose\n")
	git(t, dir, "add", "c.txt")
	git(t, dir, "commit", "-q", "-m", "second")
	w := newWorkspaces(t)
	before := hashTree(t, dir)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	sameTree(t, before, hashTree(t, dir))
	repo := w.repo("p")
	if _, err := os.Lstat(filepath.Join(repo, "objects", "info", "alternates")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the clone has alternates: %v", err)
	}
	clone := files(t, filepath.Join(repo, "objects"))
	original := files(t, filepath.Join(dir, ".git", "objects"))
	if len(clone) == 0 || len(original) == 0 {
		t.Fatalf("objects: clone %d files, original %d", len(clone), len(original))
	}
	for _, c := range clone {
		ci, err := os.Stat(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range original {
			oi, err := os.Stat(o)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(ci, oi) {
				t.Errorf("%s is %s", c, o)
			}
		}
	}
	// The worktree has the base without reaching the original's objects.
	if got := git(t, m.Path, "log", "-1", "--format=%H"); got != m.BaseCommit {
		t.Fatalf("worktree HEAD = %s, want %s", got, m.BaseCommit)
	}
}

func TestCloneFetchAndWorktreeRunNothingTheOriginalConfigures(t *testing.T) {
	t.Parallel()
	markers := tempDir(t)
	dir := armedRepo(t, markers)
	cmd := armMarkers(t, markers, "pack-objects", "upload-pack", "post-checkout", "reference-transaction", "post-commit", "pre-auto-gc")
	for key, value := range map[string]string{
		"uploadpack.packObjectsHook":     cmd["pack-objects"],
		"remote.origin.url":              fileURL(dir),
		"remote.origin.uploadpack":       cmd["upload-pack"],
		"uploadpack.advertiseBundleURIs": "true",
		"transfer.bundleURI":             "true",
		"bundle.version":                 "1",
		"bundle.mode":                    "all",
		"bundle.ada.uri":                 "ssh://example.invalid/ada.bundle",
		"uploadpack.hideRefs":            "refs/heads", // honoured only if upload-pack reads the original's config
		"core.hooksPath":                 shPath(filepath.Join(dir, ".git", "hooks")),
	} {
		git(t, dir, "config", key, value)
	}
	for _, hook := range []string{"post-checkout", "reference-transaction", "post-commit", "pre-auto-gc"} {
		path := filepath.Join(dir, ".git", "hooks", hook)
		write(t, path, "#!/bin/sh\n: > '"+shPath(filepath.Join(markers, hook+".marker"))+"'\n")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "file.txt"), "tracked\nedited\n")
	before := hashTree(t, dir)
	w := newWorkspaces(t)
	// The first prepare clones, the second fetches; each applies and commits through the filters.
	first := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir, "untracked.txt"), "1")
	second := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir, "untracked.txt"), "2")
	if got := markersWritten(t, markers); len(got) != 0 {
		t.Fatalf("Ada ran commands the repository configures: %v", got)
	}
	sameTree(t, before, hashTree(t, dir))
	if got := git(t, w.repo("p"), "rev-parse", "refs/heads/main"); got != git(t, dir, "rev-parse", "main") {
		t.Fatalf("the clone's main = %q", got)
	}
	for _, m := range []Manifest{first, second} {
		if got, err := os.ReadFile(filepath.Join(m.Path, "file.txt")); err != nil || string(got) != "tracked\nedited\n" {
			t.Fatalf("%s: file.txt = %q, %v", m.Attempt, got, err)
		}
	}
	config, err := os.ReadFile(filepath.Join(w.repo("p"), "config"))
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(config))
	if hooks := "hookspath = " + strings.ToLower(quoteConfig(filepath.ToSlash(w.Inspector.Git.hooks))); !strings.Contains(lower, hooks) || !strings.Contains(lower, `fsmonitor = false`) {
		t.Errorf("the clone's config does not point hooks at Ada's empty directory and turn fsmonitor off:\n%s", config)
	}
	for _, line := range strings.Split(lower, "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		name = strings.TrimSpace(name)
		for _, banned := range []string{"[remote", "[bundle", "[uploadpack", "[filter", "[diff", "[transfer", "sshcommand", "url", "uploadpack"} {
			if strings.HasPrefix(name, banned) {
				t.Errorf("the clone's config names %q:\n%s", name, config)
			}
		}
	}
}

func TestTheImportedChangesCommitReproducesTheSnapshot(t *testing.T) {
	t.Parallel()
	dir, head := workRepo(t)
	w := newWorkspaces(t)
	c := mustCapture(t, w.Inspector, dir, "u/bin.raw", "u/crlf.txt")
	before := hashTree(t, dir)
	m := mustPrepare(t, w, dir, c, "1")
	sameTree(t, before, hashTree(t, dir))
	if m.Branch != "ada/t1" || m.BaseCommit != head || m.Path != w.WorktreePath("p", "a1") || m.Version != ManifestVersion {
		t.Fatalf("manifest = %+v", m)
	}
	for _, p := range []string{"text.txt", "bin.dat", "crlf.txt", "gone.txt", "new.txt", "u/bin.raw", "u/crlf.txt"} {
		want, wantErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		got, gotErr := os.ReadFile(filepath.Join(m.Path, filepath.FromSlash(p)))
		if (wantErr != nil) != (gotErr != nil) || !bytes.Equal(want, got) {
			t.Errorf("%s: original %q (%v), worktree %q (%v)", p, want, wantErr, got, gotErr)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Path, "skip.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an unselected file is in the worktree: %v", err)
	}
	if got := git(t, m.Path, "log", "-1", "--format=%s%n%an <%ae>%n%cn <%ce>%n%P"); got != "Imported working changes\nAda <ada@adanext.invalid>\nAda <ada@adanext.invalid>\n"+head {
		t.Fatalf("commit = %q", got)
	}
	if got := git(t, m.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ada/t1" {
		t.Fatalf("branch = %s", got)
	}
	if got := git(t, m.Path, "status", "--porcelain"); got != "" {
		t.Fatalf("the worktree is not clean:\n%s", got)
	}
	// The commit is the original's staged and unstaged work, and the selected files.
	tracked := git(t, dir, "diff", "--binary", "HEAD")
	committed := git(t, m.Path, "diff", "--binary", head, "HEAD", "--", ".", ":(exclude)u")
	if tracked != committed {
		t.Fatalf("committed diff differs from the original's:\n%s\n---\n%s", committed, tracked)
	}
	if got := git(t, m.Path, "diff", "--name-only", head, "HEAD", "--", "u"); got != "u/bin.raw\nu/crlf.txt" {
		t.Fatalf("untracked files committed: %q", got)
	}
}

func TestASnapshotWithoutChangesMakesNoCommit(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	c := mustCapture(t, w.Inspector, dir)
	m := mustPrepare(t, w, dir, c, "1")
	if got := git(t, m.Path, "rev-parse", "HEAD"); got != c.Inspection.Identity.Head {
		t.Fatalf("HEAD = %s, want the base %s", got, c.Inspection.Identity.Head)
	}
}

func TestAnUnbornBaseIsRefused(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	git(t, dir, "add", "a.txt")
	w := newWorkspaces(t)
	c := mustCapture(t, w.Inspector, dir)
	if c.Inspection.Identity.Head != "" {
		t.Fatalf("head = %q", c.Inspection.Identity.Head)
	}
	if _, err := prepareCapture(w, dir, c, "1"); !errors.Is(err, ErrUnbornBase) {
		t.Fatalf("prepare = %v", err)
	}
	if _, err := os.Lstat(w.repo("p")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an unborn base was cloned: %v", err)
	}
}

func TestADetachedHeadNoBranchReachesGetsItsTaskBranchOnEachImport(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	for _, id := range []string{"1", "2"} {
		// A commit only the detached HEAD reaches, made anew before each import.
		git(t, dir, "checkout", "-q", "--detach")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "detached "+id)
		c := mustCapture(t, w.Inspector, dir)
		if !c.Inspection.Identity.Detached || git(t, dir, "for-each-ref", "--contains", "HEAD", "refs/heads", "refs/tags") != "" {
			t.Fatalf("not detached on a commit no branch reaches: %+v", c.Inspection.Identity)
		}
		m := mustPrepare(t, w, dir, c, id)
		if got := git(t, m.Path, "rev-parse", "HEAD"); got != c.Inspection.Identity.Head {
			t.Fatalf("import %s: HEAD = %s, want %s", id, got, c.Inspection.Identity.Head)
		}
	}
}

func TestATrackedSymlinkCannotRedirectAnUntrackedWrite(t *testing.T) {
	t.Parallel()
	outside := tempDir(t)
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	git(t, dir, "config", "core.symlinks", "true")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		// Windows without the symlink privilege; createUnder's junction case covers it.
		t.Skipf("symlinks unavailable: %v", err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "a link")
	w := newWorkspaces(t)
	// Built by hand: a capture refuses the link as an escaping_link blocker,
	// and the workspace must hold without that check.
	c := Capture{Untracked: []File{{Path: "link/evil.txt", Mode: "100644", Content: []byte("redirected\n")}}}
	c.Inspection.Identity.Head = git(t, dir, "rev-parse", "HEAD")
	if _, err := prepareCapture(w, dir, c, "1"); !errors.Is(err, errLink) {
		t.Fatalf("an untracked write through a tracked symlink: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("written outside the worktree: %v %v", entries, err)
	}
}

func TestCreateUnderRefusesALinkOnThePathAndAnExistingFile(t *testing.T) {
	t.Parallel()
	root, outside := tempDir(t), tempDir(t)
	makeLink(t, outside, filepath.Join(root, "j"))
	for _, rel := range []string{"j/evil.txt", "j/deeper/evil.txt"} {
		if err := createUnder(root, rel, false, []byte("x")); err == nil {
			t.Errorf("%s: written through a link", rel)
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("written beyond the link: %v %v", entries, err)
	}
	write(t, filepath.Join(root, "kept.txt"), "kept\n")
	if err := createUnder(root, "kept.txt", false, []byte("x")); err == nil {
		t.Error("an existing file was overwritten")
	}
	if err := createUnder(root, "new/dir/file.bin", true, []byte("\x00\r\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "new", "dir", "file.bin")); err != nil || string(got) != "\x00\r\n" {
		t.Fatalf("created %q, %v", got, err)
	}
}

func TestTwoConcurrentPreparesInOneProjectCloneOnce(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	var mu sync.Mutex
	clones, syncs := 0, 0
	w.AfterSync = func(_ context.Context, cloned bool) {
		mu.Lock()
		defer mu.Unlock()
		syncs++
		if cloned {
			clones++
		}
	}
	c := mustCapture(t, w.Inspector, dir)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = prepareCapture(w, dir, c, string(rune('1'+i))) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	if clones != 1 || syncs != 2 {
		t.Fatalf("%d clones in %d syncs", clones, syncs)
	}
}

func TestCheckFindsATamperedManifestAndAMissingWorktreeAndRebuildsNothing(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	c := mustCapture(t, w.Inspector, dir)
	m := mustPrepare(t, w, dir, c, "1")
	if err := w.Check(m, m.Hash()); err != nil {
		t.Fatalf("an untouched workspace: %v", err)
	}
	isMismatch := func(err error) bool {
		_, ok := errors.AsType[*MismatchError](err)
		return ok
	}
	// The row's copy: a field changed, with or without its hash.
	changed := m
	changed.BaseCommit = strings.Repeat("0", 40)
	if err := w.Check(changed, m.Hash()); !isMismatch(err) {
		t.Fatalf("a changed row field: %v", err)
	}
	if err := w.Check(changed, changed.Hash()); !isMismatch(err) {
		t.Fatalf("a changed row with its new hash: %v", err)
	}
	// The worktree's copy.
	copyPath := filepath.Join(w.adminDir("p", "a1"), manifestFile)
	tampered := bytes.Replace(m.Encode(), []byte(`"policy":"auto"`), []byte(`"policy":"yolo"`), 1)
	if err := os.WriteFile(copyPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(m, m.Hash()); !isMismatch(err) {
		t.Fatalf("a tampered copy: %v", err)
	}
	if got, _ := os.ReadFile(copyPath); !bytes.Equal(got, tampered) {
		t.Fatal("the tampered copy was rewritten")
	}
	if err := os.WriteFile(copyPath, m.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(m.Path); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(m, m.Hash()); !isMismatch(err) {
		t.Fatalf("a deleted worktree: %v", err)
	}
	if _, err := os.Lstat(m.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the worktree was rebuilt: %v", err)
	}
}

func TestAFetchMovesTheUsersBranchesAndLeavesAdasOwn(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "README.md"), "edited\n")
	w := newWorkspaces(t)
	first := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	imported := git(t, first.Path, "rev-parse", "HEAD")
	git(t, dir, "commit", "-q", "-am", "moved on")
	git(t, dir, "branch", "ada/t1", "HEAD~1") // the user's own branch of that name
	c := mustCapture(t, w.Inspector, dir)
	second := mustPrepare(t, w, dir, c, "2")
	repo := w.repo("p")
	if got := git(t, repo, "rev-parse", "refs/heads/main"); got != c.Inspection.Identity.Head {
		t.Fatalf("main = %s, want %s", got, c.Inspection.Identity.Head)
	}
	if got := git(t, repo, "rev-parse", "refs/heads/ada/t1"); got != imported {
		t.Fatalf("Ada's branch ada/t1 = %s, want %s", got, imported)
	}
	if got := git(t, second.Path, "rev-parse", "HEAD"); got != c.Inspection.Identity.Head {
		t.Fatalf("second worktree HEAD = %s", got)
	}
}

func TestTheBaseSurvivesAMovedHeadBeforeTheFirstPrepare(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		before, move func(t *testing.T, dir string)
	}{
		{"a detached commit, then a checkout of main", func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "--detach")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "detached")
		}, func(t *testing.T, dir string) { git(t, dir, "checkout", "-q", "main") }},
		{"a commit on main, then amended", func(*testing.T, string) {}, func(t *testing.T, dir string) {
			git(t, dir, "commit", "-q", "--amend", "--allow-empty", "-m", "amended")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			c.before(t, dir)
			w := newWorkspaces(t)
			snap := mustCapture(t, w.Inspector, dir)
			c.move(t, dir)
			if git(t, dir, "for-each-ref", "--contains", snap.Inspection.Identity.Head) != "" {
				t.Fatal("a ref still reaches the base")
			}
			m := mustPrepare(t, w, dir, snap, "1")
			if got := git(t, m.Path, "rev-parse", "HEAD"); got != snap.Inspection.Identity.Head {
				t.Fatalf("HEAD = %s, want the base %s", got, snap.Inspection.Identity.Head)
			}
		})
	}
}

func TestABranchReplacedByADirectoryOfTheSameNameStillFetches(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	git(t, dir, "branch", "feature")
	w := newWorkspaces(t)
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	git(t, dir, "branch", "-D", "feature")
	git(t, dir, "branch", "feature/x")
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "2")
	repo := w.repo("p")
	if got := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads"); got != "refs/heads/ada/t1\nrefs/heads/ada/t2\nrefs/heads/feature/x\nrefs/heads/main" {
		t.Fatalf("the clone's branches:\n%s", got)
	}
}

func TestStaleLockFilesInTheCloneAreRemovedBeforeAFetch(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	repo := w.repo("p")
	locks := []string{filepath.Join(repo, "refs", "heads", "main.lock"), filepath.Join(repo, "packed-refs.lock")}
	for _, l := range locks {
		write(t, l, "")
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "moved on")
	c := mustCapture(t, w.Inspector, dir)
	mustPrepare(t, w, dir, c, "2")
	if got := git(t, repo, "rev-parse", "refs/heads/main"); got != c.Inspection.Identity.Head {
		t.Fatalf("main = %s, want %s", got, c.Inspection.Identity.Head)
	}
	for _, l := range locks {
		if _, err := os.Lstat(l); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v", l, err)
		}
	}
}

func TestTheImportedChangesCommitReproducesTheSnapshotUnderAutocrlf(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	git(t, dir, "config", "core.autocrlf", "true")
	write(t, filepath.Join(dir, "text.txt"), "one\r\ntwo\r\n")
	write(t, filepath.Join(dir, "bin.dat"), "\x00\x01\r\n\x00")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	write(t, filepath.Join(dir, "text.txt"), "one\r\ntwo\r\nstaged\r\n")
	write(t, filepath.Join(dir, "new.txt"), "new\r\nfile\r\n")
	git(t, dir, "add", "text.txt", "new.txt")
	write(t, filepath.Join(dir, "text.txt"), "one\r\ntwo\r\nstaged\r\nunstaged\r\n")
	write(t, filepath.Join(dir, "bin.dat"), "\x00\x02\r\n\x00")
	write(t, filepath.Join(dir, "u", "crlf.txt"), "x\r\ny\r\n")
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir, "u/crlf.txt"), "1")
	for _, p := range []string{"text.txt", "bin.dat", "new.txt", "u/crlf.txt"} {
		want, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if got, err := os.ReadFile(filepath.Join(m.Path, filepath.FromSlash(p))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: worktree %q, original %q, %v", p, got, want, err)
		}
	}
	if got := git(t, m.Path, "status", "--porcelain"); got != "" {
		t.Fatalf("the worktree is not clean:\n%s", got)
	}
}

func TestAShallowOriginalThatGainsAShallowRootStillFetches(t *testing.T) {
	t.Parallel()
	upstream := newRepo(t, nil)
	git(t, upstream, "commit", "-q", "--allow-empty", "-m", "second")
	git(t, upstream, "branch", "other", "HEAD~1")
	git(t, upstream, "checkout", "-q", "other")
	git(t, upstream, "commit", "-q", "--allow-empty", "-m", "other")
	git(t, upstream, "checkout", "-q", "main")
	dir := filepath.Join(tempDir(t), "shallow")
	git(t, upstream, "clone", "-q", "--depth", "1", fileURL(upstream), dir)
	w := newWorkspaces(t)
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	git(t, dir, "fetch", "-q", "--depth", "1", "origin", "other:other")
	c := mustCapture(t, w.Inspector, dir)
	mustPrepare(t, w, dir, c, "2")
	if got, want := git(t, w.repo("p"), "rev-parse", "refs/heads/other"), git(t, dir, "rev-parse", "other"); got != want {
		t.Fatalf("other = %s, want %s", got, want)
	}
}

func TestTheCloneRunsNoAutomaticGcOrMaintenance(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		if got, _ := fixtureGit(w.repo("p"), "config", "--get", key); strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %s", key, got, want)
		}
	}
}

func TestCheckFindsAMovedOrDetachedBranchAndAReplacedImport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		move func(t *testing.T, worktree string)
	}{
		{"the branch moved back to the base", func(t *testing.T, wt string) { git(t, wt, "reset", "-q", "--soft", "HEAD~1") }},
		{"the imported commit replaced", func(t *testing.T, wt string) { git(t, wt, "commit", "-q", "--amend", "-m", "replaced") }},
		{"the worktree detached", func(t *testing.T, wt string) { git(t, wt, "checkout", "-q", "--detach") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			write(t, filepath.Join(dir, "README.md"), "edited\n")
			w := newWorkspaces(t)
			m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
			if head := git(t, m.Path, "rev-parse", "HEAD"); m.Head != head || head == m.BaseCommit {
				t.Fatalf("manifest head %q, worktree HEAD %s, base %s", m.Head, head, m.BaseCommit)
			}
			c.move(t, m.Path)
			row := m
			row.Head = "" // the row records no head: the copy's, bound by the hash, is used
			if _, ok := errors.AsType[*MismatchError](w.Check(row, m.Hash())); !ok {
				t.Fatal("Check passed a worktree that left its prepared commit")
			}
		})
	}
}
