package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// agentCommit commits what the agent left in m's worktree as Ada does, returning the commit.
func agentCommit(t *testing.T, w *Workspaces, m Manifest) string {
	t.Helper()
	row := m
	row.Head = ""
	tested, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return tested
}

// gitDiff is git's own patch for paths between two commits of the worktree's clone.
func gitDiff(t *testing.T, dir, from, to string, paths ...string) string {
	t.Helper()
	args := append([]string{"diff", "-M" + RenameThreshold, "--no-color", "--no-relative", "--full-index", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", from, to, "--"}, paths...)
	out, err := fixtureGit(dir, args...)
	if err != nil {
		t.Fatalf("git diff: %v %s", err, out)
	}
	return string(out)
}

func TestChangesMatchGitsDiffForATextChangeABinaryARenameAModeChangeAndADeletion(t *testing.T) {
	t.Parallel()
	moved := strings.Repeat("a line that stays the same\n", 20)
	dir := newRepo(t, map[string]string{"text.txt": "one\ntwo\n", "data.bin": "\x00\x01\x02", "old.txt": moved, "run.sh": "echo hi\n", "gone.txt": "bye\n"})
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	write(t, filepath.Join(m.Path, "text.txt"), "one\nthree\n")
	write(t, filepath.Join(m.Path, "data.bin"), "\x00\x09\x02\x03")
	if err := os.Remove(filepath.Join(m.Path, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, m.Path, "mv", "old.txt", "new.txt")
	write(t, filepath.Join(m.Path, "new.txt"), moved+"one more\n")
	// The agent's own commit carries the mode change, which every file system can record.
	// The work tree's mode too: where git records it, Ada's commit of the work tree keeps it.
	if err := os.Chmod(filepath.Join(m.Path, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, m.Path, "update-index", "--chmod=+x", "run.sh")
	git(t, m.Path, "commit", "-q", "-m", "agent's own")
	tested := agentCommit(t, w, m)
	cfg := configHash(t, w)
	set, err := w.Changes(context.Background(), "p", m.Head, tested, cfg, DefaultChangeLimits)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]ChangedFile{}
	for _, f := range set.Files {
		byPath[f.Path] = f
	}
	want := map[string]string{"text.txt": "M", "data.bin": "M", "new.txt": "R", "run.sh": "M", "gone.txt": "D"}
	if len(set.Files) != len(want) || set.Total != len(want) || set.FilesTruncated || set.BytesTruncated {
		t.Fatalf("changes = %+v", set)
	}
	for path, status := range want {
		f, ok := byPath[path]
		if !ok || f.Status != status {
			t.Fatalf("%s: %+v", path, f)
		}
		paths := []string{path}
		if f.OldPath != "" {
			paths = []string{f.OldPath, path}
		}
		if got, want := string(f.Patch), gitDiff(t, m.Path, m.Head, tested, paths...); got != want {
			t.Errorf("%s patch:\n%s\nwant:\n%s", path, got, want)
		}
	}
	if f := byPath["data.bin"]; !f.Binary || f.Added != -1 {
		t.Errorf("binary %+v", f)
	}
	if f := byPath["new.txt"]; f.OldPath != "old.txt" || f.Similarity < 50 {
		t.Errorf("rename %+v", f)
	}
	if f := byPath["run.sh"]; f.OldMode != "100644" || f.NewMode != "100755" {
		t.Errorf("mode change %+v", f)
	}
	if f := byPath["gone.txt"]; f.NewMode != "000000" || f.Deleted != 1 {
		t.Errorf("deletion %+v", f)
	}
}

func configHash(t *testing.T, w *Workspaces) string {
	t.Helper()
	data, err := os.ReadFile(w.configCopy("p"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestEachChangesCapMarksItsTruncation(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, filepath.Join(m.Path, name), strings.Repeat(name+" changed\n", 50))
	}
	tested := agentCommit(t, w, m)
	cfg := configHash(t, w)
	changes := func(lim ChangeLimits) ChangeSet {
		set, err := w.Changes(context.Background(), "p", m.Head, tested, cfg, lim)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	if set := changes(ChangeLimits{Files: 2}); len(set.Files) != 2 || set.Total != 3 || !set.FilesTruncated || set.BytesTruncated || set.Files[1].PatchTruncated {
		t.Errorf("file cap: %+v", set)
	}
	if set := changes(ChangeLimits{FileBytes: 100}); set.BytesTruncated || len(set.Files) != 3 || !set.Files[0].PatchTruncated || len(set.Files[0].Patch) != 100 {
		t.Errorf("per-file cap: %+v", set)
	}
	whole := changes(ChangeLimits{})
	first := int64(len(whole.Files[0].Patch))
	set := changes(ChangeLimits{TotalBytes: first + 10})
	if !set.BytesTruncated || set.Files[0].PatchTruncated || !set.Files[1].PatchTruncated || len(set.Files[1].Patch) != 10 || !set.Files[2].PatchTruncated || len(set.Files[2].Patch) != 0 {
		t.Errorf("total cap: %+v", set)
	}
}

func TestChangesAreRefusedWhenTheCloneConfigDiffers(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	cfg := configHash(t, w)
	tamperConfig(t, w, m, "[diff]\n\texternal = touch-a-marker\n")
	var mm *MismatchError
	if _, err := w.Changes(context.Background(), "p", m.Head, m.Head, cfg, DefaultChangeLimits); !errors.As(err, &mm) {
		t.Fatalf("changes: %v", err)
	}
	if _, err := w.Files(context.Background(), "p", m.Head, "", cfg, 10, 10); !errors.As(err, &mm) {
		t.Fatalf("files: %v", err)
	}
}

// symlinkCommit commits, in the worktree, a symlink link pointing at target, made in the index
// alone so no file system privilege is needed.
func symlinkCommit(t *testing.T, w *Workspaces, m Manifest, link, target string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = m.Path, fixtureEnv(), strings.NewReader(target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git(t, m.Path, "update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(string(out))+","+link)
	git(t, m.Path, "commit", "-q", "-m", "a link")
	git(t, m.Path, "reset", "-q", "--hard")
	return agentCommit(t, w, m)
}

func TestFilesShowASymlinkAsALinkAndReadNothingThroughIt(t *testing.T) {
	t.Parallel()
	outside := tempDir(t)
	write(t, filepath.Join(outside, "marker.txt"), "outside\n")
	dir := newRepo(t, map[string]string{"src/main.go": "package main\n", "README.md": "hi\n"})
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	tested := symlinkCommit(t, w, m, "src/out", filepath.ToSlash(outside))
	cfg := configHash(t, w)
	read := func(path string) (FileView, error) {
		return w.Files(context.Background(), "p", tested, path, cfg, 100, 1<<20)
	}
	root, err := read("")
	if err != nil || root.Type != "tree" || len(root.Entries) != 2 {
		t.Fatalf("root = %+v %v", root, err)
	}
	src, err := read("src")
	if err != nil || len(src.Entries) != 2 || src.Entries[1].Name != "out" || src.Entries[1].Type != "symlink" || src.Entries[1].Path != "src/out" {
		t.Fatalf("src = %+v %v", src, err)
	}
	link, err := read("src/out")
	if err != nil || link.Type != "symlink" || link.Target != filepath.ToSlash(outside) || link.Content != nil {
		t.Fatalf("link = %+v %v", link, err)
	}
	for _, p := range []string{"src/out/marker.txt", "src/missing", "README.md/x", "../x", "/src", "src/"} {
		if v, err := read(p); !errors.Is(err, ErrNoSuchPath) {
			t.Errorf("%s: %+v %v", p, v, err)
		}
	}
	file, err := read("src/main.go")
	if err != nil || file.Type != "blob" || string(file.Content) != "package main\n" || file.Size != 13 || file.Truncated {
		t.Fatalf("file = %+v %v", file, err)
	}
	capped, err := w.Files(context.Background(), "p", tested, "src/main.go", cfg, 1, 4)
	if err != nil || string(capped.Content) != "pack" || !capped.Truncated {
		t.Fatalf("capped file = %+v %v", capped, err)
	}
	listed, err := w.Files(context.Background(), "p", tested, "", cfg, 1, 4)
	if err != nil || len(listed.Entries) != 1 || !listed.EntriesTruncated {
		t.Fatalf("capped listing = %+v %v", listed, err)
	}
}

// markedDir is a directory outside the state root holding a marker file.
func markedDir(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = tempDir(t)
	marker = filepath.Join(dir, "marker.txt")
	write(t, marker, "keep me\n")
	return dir, marker
}

func keptMarker(t *testing.T, marker string) {
	t.Helper()
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me\n" {
		t.Fatalf("the marker behind a link: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Dir(marker))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("the marked directory's mode changed: %v %v", info.Mode(), err)
	}
}

func TestRemovalLeavesTheMarkerBehindLinksInsideTheWorktree(t *testing.T) {
	t.Parallel()
	target, marker := markedDir(t)
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	makeLink(t, target, filepath.Join(m.Path, "dir-link"))
	if err := os.MkdirAll(filepath.Join(m.Path, "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeLink(t, target, filepath.Join(m.Path, "deep", "er", "link"))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(marker, filepath.Join(m.Path, "file-link")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.RemoveWorktree("p", "a1"); err != nil {
		t.Fatal(err)
	}
	keptMarker(t, marker)
	for _, p := range []string{m.Path, w.adminDir("p", "a1")} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s remains: %v", p, err)
		}
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != m.BaseCommit {
		t.Fatalf("the original moved: %s", got)
	}
}

func TestRemovalOfAWorktreeReplacedByALinkRemovesOnlyTheLink(t *testing.T) {
	t.Parallel()
	target, marker := markedDir(t)
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	if err := os.RemoveAll(m.Path); err != nil {
		t.Fatal(err)
	}
	makeLink(t, target, m.Path)
	if err := w.RemoveWorktree("p", "a1"); err != nil {
		t.Fatal(err)
	}
	keptMarker(t, marker)
	if _, err := os.Lstat(m.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link remains: %v", err)
	}
}

func TestRemovalRefusesAWorktreesDirectoryReplacedByALink(t *testing.T) {
	t.Parallel()
	target, marker := markedDir(t)
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	parent := filepath.Dir(m.Path)
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(target, "a1", "inner.txt"), "inner\n")
	makeLink(t, target, parent)
	var mm *MismatchError
	if err := w.RemoveWorktree("p", "a1"); !errors.As(err, &mm) {
		t.Fatalf("removal through a linked worktrees directory: %v", err)
	}
	keptMarker(t, marker)
	if _, err := os.Stat(filepath.Join(target, "a1", "inner.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestRemovalKeepsAnotherAttemptsAdminDirectoryWhenItsWorktreeIsMissing(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	c := mustCapture(t, w.Inspector, dir)
	first := mustPrepare(t, w, dir, c, "1")
	second := mustPrepare(t, w, dir, c, "2")
	if err := os.RemoveAll(second.Path); err != nil {
		t.Fatal(err)
	}
	if err := w.RemoveWorktree("p", first.Attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.adminDir("p", second.Attempt), manifestFile)); err != nil {
		t.Fatalf("the other attempt's admin directory: %v", err)
	}
	if _, err := os.Lstat(w.adminDir("p", first.Attempt)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the removed attempt's admin directory: %v", err)
	}
}

func TestACrashBetweenTheRemovalStepsFinishesOnTheNextTry(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	crash := errors.New("crashed")
	w.afterWorktreeRemoved = func() error { return crash }
	if err := w.RemoveWorktree("p", "a1"); !errors.Is(err, crash) {
		t.Fatalf("first try: %v", err)
	}
	if _, err := os.Lstat(m.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the worktree after the first step: %v", err)
	}
	if _, err := os.Stat(w.adminDir("p", "a1")); err != nil {
		t.Fatalf("the admin directory before the second step: %v", err)
	}
	w.afterWorktreeRemoved = nil
	for range 2 {
		if err := w.RemoveWorktree("p", "a1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(w.adminDir("p", "a1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the admin directory remains: %v", err)
	}
}

func TestContinueGivesTheNextAttemptTheBranchFromItsTip(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	write(t, filepath.Join(m.Path, "README.md"), "first attempt\n")
	tip := agentCommit(t, w, m)
	next := Manifest{Project: "p", Task: m.Task, Attempt: "a2", Snapshot: m.Snapshot, BaseCommit: m.BaseCommit, Executor: "local", Policy: "auto", ConfigSHA256: configHash(t, w)}
	// The branch is still checked out in the first worktree.
	if _, err := w.Continue(context.Background(), next, tip); err == nil {
		t.Fatal("continued while the earlier worktree holds the branch")
	}
	if err := w.RemoveWorktree("p", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Continue(context.Background(), next, m.Head); err == nil {
		t.Fatal("continued from a commit that is not the branch's tip")
	}
	got, err := w.Continue(context.Background(), next, tip)
	if err != nil {
		t.Fatal(err)
	}
	if got.Head != tip || got.Branch != "ada/"+m.Task || got.Path != w.WorktreePath("p", "a2") {
		t.Fatalf("manifest = %+v", got)
	}
	if data, err := os.ReadFile(filepath.Join(got.Path, "README.md")); err != nil || !bytes.Equal(data, []byte("first attempt\n")) {
		t.Fatalf("the first attempt's work: %q %v", data, err)
	}
	if err := w.Check(got, got.Hash()); err != nil {
		t.Fatal(err)
	}
}
