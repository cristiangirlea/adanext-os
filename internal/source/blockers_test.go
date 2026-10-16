package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEachBlockerIsReported(t *testing.T) {
	t.Parallel()
	head := func(t *testing.T, dir string) string { return git(t, dir, "rev-parse", "HEAD") }
	gitFile := func(name, content string) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string {
			write(t, filepath.Join(dir, ".git", filepath.FromSlash(name)), content)
			return dir
		}
	}
	gitDir := func(name string) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string {
			if err := os.MkdirAll(filepath.Join(dir, ".git", name), 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		}
	}
	setConfig := func(pairs ...string) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string {
			for i := 0; i < len(pairs); i += 2 {
				git(t, dir, "config", pairs[i], pairs[i+1])
			}
			return dir
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, repo string) string // returns the path to inspect
		code  string
		path  string
	}{
		{"a plain directory", func(t *testing.T, _ string) string { return tempDir(t) }, BlockNotRepository, ""},
		{"a missing directory", func(t *testing.T, _ string) string { return filepath.Join(tempDir(t), "missing") }, BlockNotRepository, ""},
		{"a subdirectory of a repository", func(t *testing.T, dir string) string {
			write(t, filepath.Join(dir, "sub", "f.txt"), "x\n")
			return filepath.Join(dir, "sub")
		}, BlockNotRepository, ""},
		{"a bare repository", func(t *testing.T, _ string) string {
			dir := tempDir(t)
			git(t, dir, "init", "-q", "--bare")
			return dir
		}, BlockBare, ""},
		{"core.bare set", setConfig("core.bare", "true"), BlockBare, "core.bare"},
		{"core.worktree set", setConfig("core.worktree", "/elsewhere"), BlockWorktree, "core.worktree"},
		{"a partial clone", setConfig("core.repositoryformatversion", "1", "extensions.partialClone", "origin"), BlockPartialClone, "extensions.partialclone"},
		{"reftable", setConfig("core.repositoryformatversion", "1", "extensions.refStorage", "reftable"), BlockReftable, "extensions.refstorage"},
		{"an unknown extension", setConfig("core.repositoryformatversion", "1", "extensions.fancy", "true"), BlockUnknownExtension, "extensions.fancy"},
		{"a split index", func(t *testing.T, dir string) string {
			git(t, dir, "update-index", "--split-index")
			return dir
		}, BlockSplitIndex, "index"},
		{"sparse checkout in config", setConfig("core.sparseCheckout", "true"), BlockSparse, "core.sparseCheckout"},
		{"a skip-worktree entry", func(t *testing.T, dir string) string {
			git(t, dir, "update-index", "--skip-worktree", "README.md")
			return dir
		}, BlockSparse, "README.md"},
		{"an assume-unchanged entry", func(t *testing.T, dir string) string {
			git(t, dir, "update-index", "--assume-unchanged", "README.md")
			return dir
		}, BlockAssumeUnchanged, "README.md"},
		{"a merge", func(t *testing.T, dir string) string { return gitFile("MERGE_HEAD", head(t, dir)+"\n")(t, dir) }, BlockMerge, "MERGE_HEAD"},
		{"a rebase (merge backend)", gitDir("rebase-merge"), BlockRebase, "rebase-merge"},
		{"a rebase (apply backend)", gitDir("rebase-apply"), BlockRebase, "rebase-apply"},
		{"a cherry-pick", func(t *testing.T, dir string) string { return gitFile("CHERRY_PICK_HEAD", head(t, dir)+"\n")(t, dir) }, BlockCherryPick, "CHERRY_PICK_HEAD"},
		{"a paused cherry-pick sequence", gitFile("sequencer/todo", "pick 0123456 subject\n"), BlockCherryPick, "sequencer"},
		{"a revert", func(t *testing.T, dir string) string { return gitFile("REVERT_HEAD", head(t, dir)+"\n")(t, dir) }, BlockRevert, "REVERT_HEAD"},
		{"a paused revert sequence", gitFile("sequencer/todo", "revert 0123456 subject\n"), BlockRevert, "sequencer"},
		{"a bisect", gitFile("BISECT_LOG", "git bisect start\n"), BlockBisect, "BISECT_LOG"},
		{"an unmerged entry", func(t *testing.T, dir string) string {
			git(t, dir, "checkout", "-q", "-b", "other")
			write(t, filepath.Join(dir, "README.md"), "other\n")
			git(t, dir, "commit", "-q", "-am", "other")
			git(t, dir, "checkout", "-q", "main")
			write(t, filepath.Join(dir, "README.md"), "main\n")
			git(t, dir, "commit", "-q", "-am", "main")
			fixtureGit(dir, "merge", "other") // conflicts, as intended
			return dir
		}, BlockUnmerged, "README.md"},
		{"a gitlink", func(t *testing.T, dir string) string {
			git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head(t, dir)+",vendor/lib")
			return dir
		}, BlockSubmodule, "vendor/lib"},
		{"filter=lfs in a tracked .gitattributes", func(t *testing.T, dir string) string {
			write(t, filepath.Join(dir, ".gitattributes"), "# large files\n*.bin filter=lfs diff=lfs merge=lfs -text\n")
			git(t, dir, "add", ".gitattributes")
			return dir
		}, BlockLFS, ".gitattributes"},
		{"filter=lfs in an untracked nested .gitattributes", func(t *testing.T, dir string) string {
			write(t, filepath.Join(dir, "assets", ".gitattributes"), "*.psd filter=lfs\n")
			return dir
		}, BlockLFS, "assets/.gitattributes"},
		{"filter=lfs in info/attributes", gitFile("info/attributes", "*.iso filter=lfs\n"), BlockLFS, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := c.setup(t, newRepo(t, nil))
			r := inspect(t, newInspector(t), path)
			b, ok := blocker(r, c.code)
			if !ok {
				t.Fatalf("no %s blocker in %+v", c.code, r.Blockers)
			}
			if c.path != "" && !hasPath(b.Paths, c.path) {
				t.Fatalf("%s paths = %v, want %s", c.code, b.Paths, c.path)
			}
		})
	}
}

func TestACleanRepositoryHasNoBlockers(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"AGENTS.md": "rules\n", "go.mod": "module x\n", "readme.txt": "hi\n", "src/main.go": "package main\n"})
	git(t, dir, "remote", "add", "origin", "https://example.com/x.git")
	r := inspect(t, newInspector(t), dir)
	if len(r.Blockers) != 0 {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
	if r.Identity.Path != dir || r.Identity.Branch != "main" || r.Identity.Detached || len(r.Identity.Head) != 40 {
		t.Fatalf("identity = %+v", r.Identity)
	}
	if len(r.Identity.Remotes) != 1 || r.Identity.Remotes[0] != (Remote{"origin", "https://example.com/x.git"}) {
		t.Fatalf("remotes = %+v", r.Identity.Remotes)
	}
	if strings.Join(r.Files, ",") != "AGENTS.md,go.mod,readme.txt" {
		t.Fatalf("files = %v", r.Files)
	}
	if len(r.History) != 1 || r.History[0].Commit != r.Identity.Head || r.History[0].Subject != "first" {
		t.Fatalf("history = %+v", r.History)
	}
	git(t, dir, "checkout", "-q", "--detach")
	if r := inspect(t, newInspector(t), dir); !r.Identity.Detached || r.Identity.Branch != "" {
		t.Fatalf("detached identity = %+v", r.Identity)
	}
}

func TestAFreshRepositoryWithoutCommitsInspects(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	r := inspect(t, newInspector(t), dir)
	if len(r.Blockers) != 0 || r.Identity.Head != "" || r.Identity.Branch != "main" || len(r.History) != 0 || !hasPath(r.Status.Untracked, "a.txt") {
		t.Fatalf("inspection = %+v", r)
	}
}

func TestALinkInsideTheWorkTreeIsNoBlocker(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"inside/file.txt": "x\n"})
	makeLink(t, filepath.Join(dir, "inside"), filepath.Join(dir, "link"))
	r := inspect(t, newInspector(t), dir)
	if _, ok := blocker(r, BlockEscapingLink); ok {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestALinkLeavingTheWorkTreeIsABlocker(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	outside := tempDir(t)
	write(t, filepath.Join(outside, "secret.txt"), "outside\n") // git lists only what it can see through the link
	makeLink(t, outside, filepath.Join(dir, "escape"))
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || !hasPath(b.Paths, "escape") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestATrackedSymlinkLeavingTheWorkTreeIsABlocker(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs a privilege CI does not have; junctions cover Windows")
	}
	dir := newRepo(t, nil)
	if err := os.Symlink("../../etc", filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "up")
	git(t, dir, "commit", "-q", "-m", "link")
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || !hasPath(b.Paths, "up") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestALinkThroughAnIgnoredLinkThatLeavesIsABlocker(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{".gitignore": "hidden\n"})
	outside := tempDir(t)
	write(t, filepath.Join(outside, "secret.txt"), "outside\n")
	makeLink(t, outside, filepath.Join(dir, "hidden"))
	makeLink(t, filepath.Join(dir, "hidden"), filepath.Join(dir, "visible"))
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || !hasPath(b.Paths, "visible") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestALinkOutsideThatLeadsBackInIsABlocker(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"inside/file.txt": "x\n"})
	outside := tempDir(t)
	makeLink(t, filepath.Join(dir, "inside"), filepath.Join(outside, "back"))
	makeLink(t, filepath.Join(outside, "back"), filepath.Join(dir, "away"))
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || !hasPath(b.Paths, "away") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestNothingIsReadThroughALinkInTheWorkTree(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"sub/.gitattributes": "*.txt text\n", "sub/a.txt": "a\n"})
	outside := tempDir(t)
	write(t, filepath.Join(outside, ".gitattributes"), "*.bin filter=lfs\n")
	write(t, filepath.Join(outside, "a.txt"), "outside\n")
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	makeLink(t, outside, filepath.Join(dir, "sub"))
	in := newInspector(t)
	r := inspect(t, in, dir)
	if _, ok := blocker(r, BlockLFS); ok {
		t.Fatalf("an attributes file was read through the link: %+v", r.Blockers)
	}
	if _, ok := blocker(r, BlockEscapingLink); !ok {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
	write(t, filepath.Join(outside, "a.txt"), "changed outside\n")
	if again := inspect(t, in, dir); again.Fingerprint != r.Fingerprint {
		t.Fatal("the fingerprint hashed a file through the link")
	}
}

// refused is an inspection that ends in a not_git_repository blocker or a
// link refused on open.
func refused(t *testing.T, in *Inspector, path string) bool {
	t.Helper()
	r, err := in.Inspect(context.Background(), path)
	_, blocked := blocker(r, BlockNotRepository)
	return blocked || errors.Is(err, errLink) || errors.Is(err, syscall.ELOOP)
}

// moveAndLink moves dir away and leaves a link to it in its place.
func moveAndLink(t *testing.T, dir string) {
	t.Helper()
	moved := filepath.Join(tempDir(t), filepath.Base(dir))
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	makeLink(t, moved, dir)
}

func TestLinksInTheGitDirectoryAreRefused(t *testing.T) {
	t.Parallel()
	for _, part := range []string{"refs", "objects", "info", "refs/heads", "objects/info"} {
		t.Run(part, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			moveAndLink(t, filepath.Join(dir, ".git", filepath.FromSlash(part)))
			if !refused(t, newInspector(t), dir) {
				t.Fatalf("a linked .git/%s was followed", part)
			}
		})
	}
	t.Run("an alternate", func(t *testing.T) {
		t.Parallel()
		dir, other := newRepo(t, nil), newRepo(t, nil)
		link := filepath.Join(tempDir(t), "objects")
		makeLink(t, filepath.Join(other, ".git", "objects"), link)
		write(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), filepath.ToSlash(link)+"\n")
		if !refused(t, newInspector(t), dir) {
			t.Fatal("a linked alternate was followed")
		}
	})
	for _, part := range []string{"refs", "info"} {
		t.Run("a linked worktree's "+part, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			wt := filepath.Join(tempDir(t), "wt")
			git(t, dir, "worktree", "add", "-q", "-b", "feature", wt)
			linked := filepath.Join(dir, ".git", "worktrees", "wt", part)
			if err := os.RemoveAll(linked); err != nil {
				t.Fatal(err)
			}
			makeLink(t, tempDir(t), linked)
			if !refused(t, newInspector(t), wt) {
				t.Fatalf("a linked worktree's %s was followed", part)
			}
		})
	}
}

func TestChangedPathsUnderOneLinkReportOnlyThatLink(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"sub/a.txt": "a\n", "sub/b.txt": "b\n"})
	outside, further := tempDir(t), tempDir(t)
	// Links beneath the link are found only by looking through it.
	makeLink(t, further, filepath.Join(outside, "a.txt"))
	makeLink(t, further, filepath.Join(outside, "b.txt"))
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	makeLink(t, outside, filepath.Join(dir, "sub"))
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || strings.Join(b.Paths, ",") != "sub" {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

// fileInfo is an os.FileInfo with only a mode.
type fileInfo struct{ mode fs.FileMode }

func (f fileInfo) Name() string       { return "f" }
func (f fileInfo) Size() int64        { return 0 }
func (f fileInfo) Mode() fs.FileMode  { return f.mode }
func (f fileInfo) ModTime() time.Time { return time.Time{} }
func (f fileInfo) IsDir() bool        { return false }
func (f fileInfo) Sys() any           { return nil }

func TestAnIrregularFileReadlinkCannotReadIsNotALink(t *testing.T) {
	t.Parallel()
	// A cloud or dedup reparse point is irregular, and Readlink fails on it.
	file := filepath.Join(tempDir(t), "f")
	write(t, file, "x\n")
	if isLink(file, fileInfo{fs.ModeIrregular}) {
		t.Fatal("an irregular file Readlink cannot read is a link")
	}
	if !isLink(file, fileInfo{fs.ModeSymlink}) {
		t.Fatal("a symlink is not a link")
	}
}

func TestAnOversizedConfigIsAnError(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	config := filepath.Join(dir, ".git", "config")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	write(t, config, string(data)+"#"+strings.Repeat("a", 16<<20)+"\n")
	if _, err := newInspector(t).Inspect(context.Background(), dir); err == nil {
		t.Fatal("a config over 16 MiB was read")
	}
}

func TestALinkedWorktreeInspects(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	wt := filepath.Join(tempDir(t), "wt")
	git(t, dir, "worktree", "add", "-q", "-b", "feature", wt)
	write(t, filepath.Join(wt, "new.txt"), "new\n")
	r := inspect(t, newInspector(t), wt)
	if len(r.Blockers) != 0 {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
	gitDir, commonDir := filepath.Join(dir, ".git", "worktrees", "wt"), filepath.Join(dir, ".git")
	if !strings.EqualFold(r.Identity.GitDir, gitDir) || !strings.EqualFold(r.Identity.CommonDir, commonDir) {
		t.Fatalf("identity = %+v", r.Identity)
	}
	if r.Identity.Branch != "feature" || r.Identity.Head != git(t, dir, "rev-parse", "main") || len(r.History) != 1 {
		t.Fatalf("identity = %+v, history = %+v", r.Identity, r.History)
	}
	if !hasPath(r.Status.Untracked, "new.txt") || len(r.Status.Staged)+len(r.Status.Unstaged) != 0 {
		t.Fatalf("status = %+v", r.Status)
	}
}

// makeLink makes a directory link: a symlink, or on Windows a junction,
// which needs no privilege.
func makeLink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v: %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestRemotePathsAreRefusedUnopened(t *testing.T) {
	// Not parallel: it records openFile, which parallel tests also use.
	var opened []string
	openFile = func(path string) (*os.File, error) {
		opened = append(opened, path)
		return openShared(path)
	}
	t.Cleanup(func() { openFile = openShared })
	remotes := []string{`\\host\share\repo`, `//host/share/repo`, `\\?\C:\repo`, `\\.\pipe\repo`}
	type layoutCase struct {
		name    string
		setup   func(t *testing.T, remote string) string
		blocked bool // refused with a unc_path blocker
	}
	repoConfig := func(key string) func(t *testing.T, remote string) string {
		return func(t *testing.T, remote string) string {
			dir := newRepo(t, nil)
			git(t, dir, "config", key, remote)
			return dir
		}
	}
	cases := []layoutCase{
		{"gitdir", func(t *testing.T, remote string) string {
			dir := tempDir(t)
			write(t, filepath.Join(dir, ".git"), "gitdir: "+remote+"\n")
			return dir
		}, true},
		{"commondir", func(t *testing.T, remote string) string {
			dir, linked := tempDir(t), tempDir(t)
			write(t, filepath.Join(linked, "HEAD"), "ref: refs/heads/main\n")
			write(t, filepath.Join(linked, "commondir"), remote+"\n")
			write(t, filepath.Join(dir, ".git"), "gitdir: "+linked+"\n")
			return dir
		}, true},
		{"alternates", func(t *testing.T, remote string) string {
			dir := newRepo(t, nil)
			write(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), remote+"\n")
			return dir
		}, true},
		// The repository config's own excludes and attributes files: skipped, never opened.
		{"core.excludesFile", repoConfig("core.excludesFile"), false},
		{"core.attributesFile", repoConfig("core.attributesFile"), false},
	}
	in := newInspector(t)
	for _, c := range cases {
		for _, remote := range remotes {
			t.Run(c.name+" "+remote, func(t *testing.T) {
				path := c.setup(t, remote)
				opened = nil
				r := inspect(t, in, path)
				for _, p := range opened {
					if IsRemotePath(p) {
						t.Fatalf("opened %s", p)
					}
				}
				if !c.blocked {
					return
				}
				if b, ok := blocker(r, BlockRemotePath); !ok || !hasPath(b.Paths, remote) {
					t.Fatalf("blockers = %+v", r.Blockers)
				}
			})
		}
	}
}

func TestIsRemotePathKnowsUNCAndDevicePaths(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]bool{
		`\\host\share\repo`: true, `//host/share/repo`: true, `\\?\C:\repo`: true, `//?/C:/repo`: true,
		`\\.\pipe\x`: true, `\??\C:\repo`: true, `\??\UNC\host\share`: true,
		`C:\repo`: false, `C:/repo`: false, `/home/repo`: false, `relative`: false, `\repo`: false, `/a//b`: false,
	} {
		if got := IsRemotePath(p); got != want {
			t.Errorf("IsRemotePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestInspectRefusesARelativePath(t *testing.T) {
	t.Parallel()
	if _, err := newInspector(t).Inspect(context.Background(), "relative/repo"); err == nil {
		t.Fatal("a relative path was inspected")
	}
}
