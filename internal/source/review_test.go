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

// indexCommit commits, in the worktree, an entry of mode at path whose object is oid (a blob's
// content for a symlink, a commit id for a gitlink), then checks the worktree out to match.
func indexCommit(t *testing.T, m Manifest, mode, path, oid string) {
	t.Helper()
	git(t, m.Path, "update-index", "--add", "--cacheinfo", mode+","+oid+","+path)
	git(t, m.Path, "commit", "-q", "-m", "agent's "+mode)
	git(t, m.Path, "reset", "-q", "--hard")
}

func blobOf(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = dir, fixtureEnv(), strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func changesBetween(t *testing.T, w *Workspaces, from, to string) ChangeSet {
	t.Helper()
	set, err := w.Changes(context.Background(), "p", from, to, configHash(t, w), DefaultChangeLimits)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestChangesKeepEachPatchWithItsFileAfterATypeChange(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "regular\n", "b.txt": "b\n"})
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	write(t, filepath.Join(m.Path, "b.txt"), "b changed\n")
	git(t, m.Path, "add", "b.txt")
	// a.txt becomes a symlink: git writes its patch as a deletion and an addition.
	indexCommit(t, m, "120000", "a.txt", blobOf(t, m.Path, "b.txt"))
	tested := agentCommit(t, w, m)
	set := changesBetween(t, w, m.Head, tested)
	if len(set.Files) != 2 || set.Files[0].Path != "a.txt" || set.Files[0].Status != "T" || set.Files[1].Path != "b.txt" {
		t.Fatalf("changes = %+v", set)
	}
	for _, f := range set.Files {
		if got, want := string(f.Patch), gitDiff(t, m.Path, m.Head, tested, f.Path); got != want {
			t.Errorf("%s patch:\n%s\nwant:\n%s", f.Path, got, want)
		}
	}
}

func TestChangesShowAnAddedAndABumpedSubmodulePointer(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	first, bumped, other := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	if err := os.MkdirAll(filepath.Join(m.Path, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	indexCommit(t, m, "160000", "lib", first)
	added := agentCommit(t, w, m)
	set := changesBetween(t, w, m.Head, added)
	if len(set.Files) != 1 || set.Files[0].Path != "lib" || set.Files[0].Status != "A" || set.Files[0].NewMode != "160000" {
		t.Fatalf("an added submodule: %+v", set)
	}
	if got, want := string(set.Files[0].Patch), gitDiff(t, m.Path, m.Head, added, "lib"); got != want || !strings.Contains(got, "+Subproject commit "+first) {
		t.Fatalf("added patch:\n%s\nwant:\n%s", got, want)
	}
	if err := os.MkdirAll(filepath.Join(m.Path, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, m.Path, "update-index", "--cacheinfo", "160000,"+bumped+",lib")
	indexCommit(t, m, "160000", "other", other)
	row := m
	row.Head = ""
	tip, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	set = changesBetween(t, w, added, tip)
	if len(set.Files) != 2 || set.Files[0].Path != "lib" || set.Files[0].Status != "M" || set.Files[1].Path != "other" {
		t.Fatalf("a bumped and an added submodule: %+v", set)
	}
	if got := string(set.Files[0].Patch); !strings.Contains(got, "-Subproject commit "+first) || !strings.Contains(got, "+Subproject commit "+bumped) {
		t.Fatalf("bumped patch:\n%s", got)
	}
	view, err := w.Files(context.Background(), "p", tip, "lib", configHash(t, w), 10, 10)
	if err != nil || view.Type != "submodule" || view.Submodule != bumped {
		t.Fatalf("the submodule in files: %+v %v", view, err)
	}
	root, err := w.Files(context.Background(), "p", tip, "", configHash(t, w), 10, 10)
	if err != nil || len(root.Entries) != 3 || root.Entries[1].Name != "lib" || root.Entries[1].Submodule != bumped {
		t.Fatalf("the root listing: %+v %v", root, err)
	}
}

func TestChangesAreRefusedWhenTheCloneHoldsAttributesAdaDidNotWrite(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "a\n"})
	write(t, filepath.Join(dir, ".git", "info", "attributes"), "*.bin binary\n")
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	write(t, filepath.Join(m.Path, "a.txt"), "changed\n")
	tested := agentCommit(t, w, m)
	// The user's own attributes, copied by Ada, are no reason to refuse.
	changesBetween(t, w, m.Head, tested)
	for _, name := range []string{filepath.Join("info", "attributes"), "ada-attributes"} {
		path := filepath.Join(w.repo("p"), name)
		write(t, path, "* -diff\n")
		var mm *MismatchError
		if _, err := w.Changes(context.Background(), "p", m.Head, tested, configHash(t, w), DefaultChangeLimits); !errors.As(err, &mm) {
			t.Fatalf("%s written by an agent: %v", name, err)
		}
		if _, err := w.Files(context.Background(), "p", tested, "", configHash(t, w), 10, 10); !errors.As(err, &mm) {
			t.Fatalf("files with %s written by an agent: %v", name, err)
		}
		if name == "ada-attributes" {
			os.Remove(path)
		} else {
			write(t, path, "*.bin binary\n")
		}
	}
	changesBetween(t, w, m.Head, tested)
}

func TestRemovalOfAWorktreeHoldingAReadOnlyDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a directory's mode does not stop a removal on Windows")
	}
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	ro := filepath.Join(m.Path, "ro")
	write(t, filepath.Join(ro, "inner", "file.txt"), "x\n")
	for _, d := range []string{filepath.Join(ro, "inner"), ro} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755); os.Chmod(filepath.Join(ro, "inner"), 0o755) })
	if err := w.RemoveWorktree("p", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(m.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the worktree remains: %v", err)
	}
}

func TestListingsAndDiffRecordsStopAtTheirHardCap(t *testing.T) {
	dir := newRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n", "d.txt": "d\n"})
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		write(t, filepath.Join(m.Path, name), name+" changed\n")
	}
	tested := agentCommit(t, w, m)
	set, err := w.Changes(context.Background(), "p", m.Head, tested, configHash(t, w), ChangeLimits{Files: 1})
	if err != nil || len(set.Files) != 1 || set.Total != 4 || !set.FilesTruncated || set.TotalCapped || len(set.Files[0].Patch) == 0 {
		t.Fatalf("a listing past the file cap: %+v %v", set, err)
	}
	defer func(old int) { maxStreamRecords = old }(maxStreamRecords)
	maxStreamRecords = 2
	set, err = w.Changes(context.Background(), "p", m.Head, tested, configHash(t, w), ChangeLimits{Files: 1})
	if err != nil || len(set.Files) != 1 || !set.TotalCapped || set.Total > 2 {
		t.Fatalf("a diff past the record cap: %+v %v", set, err)
	}
	view, err := w.Files(context.Background(), "p", tested, "", configHash(t, w), 1, 10)
	if err != nil || len(view.Entries) != 1 || !view.EntriesTruncated {
		t.Fatalf("a listing past the record cap: %+v %v", view, err)
	}
	if _, err := w.Files(context.Background(), "p", tested, "d.txt", configHash(t, w), 1, 10); err == nil {
		t.Fatal("a lookup past the record cap found its entry")
	}
}

func TestAnOlderCloneWithAttributesButNoCopiesIsRefusedWithoutAMismatch(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "a\n"})
	write(t, filepath.Join(dir, ".git", "info", "attributes"), "*.bin binary\n")
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	// A clone synced before Ada kept copies of its attributes files.
	for _, path := range []string{w.attributeFiles("p")[0].copy, w.attributeFiles("p")[1].copy, w.attributeCopiesMark("p")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(m.Path, "a.txt"), "changed\n")
	row := m
	row.Head = ""
	var mm *MismatchError
	if _, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash()); !errors.Is(err, ErrNoAttributeCopies) || errors.As(err, &mm) {
		t.Fatalf("commit in an older clone: %v", err)
	}
	if _, err := w.Changes(context.Background(), "p", m.Head, m.Head, configHash(t, w), DefaultChangeLimits); !errors.Is(err, ErrNoAttributeCopies) {
		t.Fatalf("changes in an older clone: %v", err)
	}
}

func TestAttributeCopiesHoldWhatAdaWrote(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "a\n"})
	write(t, filepath.Join(dir, ".git", "info", "attributes"), "*.bin binary\n")
	w := newWorkspaces(t)
	mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	got, err := os.ReadFile(w.attributeFiles("p")[0].copy)
	if err != nil || string(got) != "*.bin binary\n" {
		t.Fatalf("the info/attributes copy: %q %v", got, err)
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestAReadErrorIsAnErrorNotACap(t *testing.T) {
	broken := errors.New("pipe broke")
	var seen []string
	complete, err := readTokens(&failingReader{data: []byte("a\x00b\x00partial"), err: broken}, func(tok []byte) bool {
		seen = append(seen, string(tok))
		return true
	})
	if !errors.Is(err, broken) || complete || strings.Join(seen, ",") != "a,b" {
		t.Fatalf("complete %v, err %v, seen %v", complete, err, seen)
	}
	complete, err = readTokens(strings.NewReader("a\x00b\x00"), func([]byte) bool { return false })
	if err != nil || complete {
		t.Fatalf("a stop by the callback: complete %v, err %v", complete, err)
	}
}
