package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// driftRepo has two commits on main, then a staged edit of a.txt, an
// unstaged edit of b.txt and an untracked notes.txt.
func driftRepo(t *testing.T) string {
	t.Helper()
	dir := newRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	write(t, filepath.Join(dir, "c.txt"), "c\n")
	git(t, dir, "add", "c.txt")
	git(t, dir, "commit", "-q", "-m", "second")
	write(t, filepath.Join(dir, "a.txt"), "a staged\n")
	git(t, dir, "add", "a.txt")
	write(t, filepath.Join(dir, "b.txt"), "b unstaged\n")
	write(t, filepath.Join(dir, "notes.txt"), "untracked\n")
	return dir
}

// baselineOf is what a snapshot taken at r records.
func baselineOf(r Inspection) Baseline {
	return Baseline{BaseCommit: r.Identity.Head, Branch: r.Identity.Branch, Paths: r.Paths}
}

func drift(t *testing.T, in *Inspector, dir string, base Baseline) Drift {
	t.Helper()
	d, err := in.Drift(context.Background(), dir, base)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	return d
}

func TestAnUnchangedOriginalReportsNoDriftAfterAStatusOnlyIndexRewrite(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	if len(base.Paths) != 3 {
		t.Fatalf("per-path entries = %+v", base.Paths)
	}
	backdate(t, filepath.Join(dir, "c.txt"))
	indexBefore, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "status")
	indexAfter, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("git status did not rewrite the index; the case is not exercised")
	}
	before := hashTree(t, dir)
	d := drift(t, in, dir, base)
	sameTree(t, before, hashTree(t, dir))
	if d.Drift || d.HeadMoved || d.BranchChanged || d.Detached || d.NewCommitCount != 0 || d.Head != base.BaseCommit || d.Branch != "main" || d.SnapshotBranch != "main" {
		t.Fatalf("drift = %+v", d)
	}
	for name, list := range map[string]int{"new_commits": len(d.NewCommits), "changed": len(d.Changed), "untracked": len(d.Untracked), "blockers": len(d.Blockers)} {
		if list != 0 {
			t.Errorf("%s has %d entries", name, list)
		}
	}
	if d.NewCommits == nil || d.Changed == nil || d.Untracked == nil || d.Blockers == nil {
		t.Fatalf("an empty list is null: %+v", d)
	}
}

func TestDriftReportsEachKindSeparately(t *testing.T) {
	t.Parallel()
	type want struct {
		commits            []string // subjects, newest first
		headMoved          bool
		branch             string
		detached           bool
		changed, untracked []string
		blockers           []string
	}
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dir string)
		want   want
	}{
		{"a new commit on the branch", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "d.txt"), "d\n")
			git(t, dir, "add", "d.txt")
			git(t, dir, "commit", "-q", "-m", "third", "--", "d.txt")
		}, want{commits: []string{"third"}, headMoved: true, branch: "main"}},
		{"HEAD moved to another commit", func(t *testing.T, dir string) {
			git(t, dir, "reset", "-q", "--soft", "HEAD~1")
		}, want{headMoved: true, branch: "main", changed: []string{"c.txt"}}},
		{"a new commit on the snapshot's branch while another is checked out", func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "-b", "other")
			tree := git(t, dir, "write-tree")
			commit := git(t, dir, "commit-tree", "-p", "main", "-m", "elsewhere", tree)
			git(t, dir, "update-ref", "refs/heads/main", commit)
		}, want{commits: []string{"elsewhere"}, branch: "other"}},
		{"another branch checked out", func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "-b", "other")
		}, want{branch: "other"}},
		{"HEAD detached", func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "--detach")
		}, want{detached: true}},
		{"a tracked file edited", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "b.txt"), "b edited\n")
		}, want{branch: "main", changed: []string{"b.txt"}}},
		{"a clean tracked file edited", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "c.txt"), "c edited\n")
		}, want{branch: "main", changed: []string{"c.txt"}}},
		{"the staging changed", func(t *testing.T, dir string) {
			git(t, dir, "add", "b.txt")
		}, want{branch: "main", changed: []string{"b.txt"}}},
		{"a staged edit reverted", func(t *testing.T, dir string) {
			git(t, dir, "reset", "-q", "--", "a.txt")
			git(t, dir, "checkout", "-q", "--", "a.txt")
		}, want{branch: "main", changed: []string{"a.txt"}}},
		{"a new untracked path", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "new", "n.txt"), "new\n")
		}, want{branch: "main", untracked: []string{"new/n.txt"}}},
		{"an untracked path edited", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "notes.txt"), "edited\n")
		}, want{branch: "main", changed: []string{"notes.txt"}}},
		{"an untracked path removed", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "notes.txt")); err != nil {
				t.Fatal(err)
			}
		}, want{branch: "main", changed: []string{"notes.txt"}}},
		{"a blocker appeared", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, ".git", "MERGE_HEAD"), git(t, dir, "rev-parse", "HEAD")+"\n")
		}, want{branch: "main", blockers: []string{BlockMerge}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := driftRepo(t)
			in := newInspector(t)
			base := baselineOf(inspect(t, in, dir))
			c.change(t, dir)
			d := drift(t, in, dir, base)
			var subjects, codes []string
			for _, commit := range d.NewCommits {
				subjects = append(subjects, commit.Subject)
			}
			for _, b := range d.Blockers {
				codes = append(codes, b.Code)
			}
			w := c.want
			if !d.Drift || !slices.Equal(subjects, w.commits) || d.NewCommitCount != len(w.commits) || d.HeadMoved != w.headMoved ||
				d.Branch != w.branch || d.Detached != w.detached || d.BranchChanged != (w.branch != "main") ||
				!slices.Equal(d.Changed, w.changed) || !slices.Equal(d.Untracked, w.untracked) || !slices.Equal(codes, w.blockers) {
				t.Fatalf("drift = %+v, want %+v", d, w)
			}
		})
	}
}

func TestDriftFromAnUnbornBranchListsTheFirstCommits(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	in := newInspector(t)
	r := inspect(t, in, dir)
	if r.Identity.Head != "" || r.Identity.Branch != "main" {
		t.Fatalf("identity = %+v", r.Identity)
	}
	base := baselineOf(r)
	if d := drift(t, in, dir, base); d.Drift {
		t.Fatalf("unchanged unborn branch: %+v", d)
	}
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "first")
	d := drift(t, in, dir, base)
	if !d.Drift || !d.HeadMoved || d.NewCommitCount != 1 || len(d.NewCommits) != 1 || d.NewCommits[0].Subject != "first" || !slices.Equal(d.Changed, []string{"a.txt"}) {
		t.Fatalf("drift = %+v", d)
	}
}

func TestDriftOfAStagedRenameAndAnExecutableBit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		change  func(t *testing.T, dir string)
		changed []string
	}{
		{"a staged rename", func(t *testing.T, dir string) {
			git(t, dir, "mv", "c.txt", "moved.txt")
		}, []string{"c.txt", "moved.txt"}},
		{"an executable bit", func(t *testing.T, dir string) {
			if runtime.GOOS == "windows" {
				t.Skip("Windows files carry no executable bit")
			}
			if err := os.Chmod(filepath.Join(dir, "c.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, []string{"c.txt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := driftRepo(t)
			in := newInspector(t)
			base := baselineOf(inspect(t, in, dir))
			c.change(t, dir)
			d := drift(t, in, dir, base)
			if !d.Drift || d.HeadMoved || d.BranchChanged || !slices.Equal(d.Changed, c.changed) || d.ChangedCount != len(c.changed) || len(d.Untracked) != 0 {
				t.Fatalf("drift = %+v", d)
			}
		})
	}
}

func TestAStoppingBlockerLeavesHeadAndBranchUnread(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, code string
		change     func(t *testing.T, dir string)
	}{
		{"the original deleted", BlockNotRepository, func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"a partial clone", BlockPartialClone, func(t *testing.T, dir string) {
			git(t, dir, "config", "core.repositoryformatversion", "1")
			git(t, dir, "config", "extensions.partialClone", "origin")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := driftRepo(t)
			in := newInspector(t)
			base := baselineOf(inspect(t, in, dir))
			c.change(t, dir)
			d := drift(t, in, dir, base)
			if !d.Drift || d.HeadMoved || d.BranchChanged || d.Head != "" || d.NewCommitCount != 0 || len(d.Blockers) != 1 || d.Blockers[0].Code != c.code {
				t.Fatalf("drift = %+v", d)
			}
		})
	}
}

func TestNewCommitsStartFromTheHeadAlreadyResolved(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	in := newInspector(t)
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "commit", "-q", "-m", "resolved")
	resolved := git(t, dir, "rev-parse", "HEAD")
	// A commit that lands after HEAD was read.
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "later")
	for name, b := range map[string]Baseline{"detached": {BaseCommit: base}, "on the checked-out branch": {BaseCommit: base, Branch: "main"}} {
		commits, count, err := in.newCommits(context.Background(), filepath.Join(dir, ".git"), resolved, "main", b)
		if err != nil || count != 1 || len(commits) != 1 || commits[0].Commit != resolved {
			t.Fatalf("%s: %d %+v, %v", name, count, commits, err)
		}
	}
}

func TestDriftListsAHundredNewCommitsAndCountsThemAll(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	var stream strings.Builder
	for i := range 105 {
		fmt.Fprintf(&stream, "commit refs/heads/main\ncommitter Ada Test <ada@example.com> %d +0000\ndata 4\nc%03d\n", 1700000000+i, i)
		if i == 0 {
			fmt.Fprintf(&stream, "from %s\n", base.BaseCommit)
		}
	}
	cmd := exec.Command("git", "fast-import", "--quiet", "--force")
	cmd.Dir, cmd.Env, cmd.Stdin = dir, fixtureEnv(), strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	d := drift(t, in, dir, base)
	if !d.Drift || !d.HeadMoved || d.NewCommitCount != 105 || len(d.NewCommits) != maxDriftCommits || d.NewCommits[0].Subject != "c104" || d.NewCommits[99].Subject != "c005" {
		t.Fatalf("drift = %d listed of %d, head moved %v", len(d.NewCommits), d.NewCommitCount, d.HeadMoved)
	}
}

func TestDriftAfterTheSnapshotsBranchIsDeletedListsTheCommitsOnHead(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	git(t, dir, "checkout", "-q", "-b", "other")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "on other")
	git(t, dir, "branch", "-q", "-D", "main")
	d := drift(t, in, dir, base)
	if !d.Drift || !d.HeadMoved || !d.BranchChanged || d.Branch != "other" || d.NewCommitCount != 1 || d.NewCommits[0].Subject != "on other" {
		t.Fatalf("drift = %+v", d)
	}
}

func TestDriftFromABaseTheOriginalPrunedListsNoCommits(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	git(t, dir, "update-ref", "refs/heads/main", "HEAD~1")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "rewritten")
	git(t, dir, "reflog", "expire", "--expire=now", "--all")
	git(t, dir, "gc", "-q", "--prune=now")
	if _, err := fixtureGit(dir, "cat-file", "-e", base.BaseCommit); err == nil {
		t.Fatal("the base survived the prune; the case is not exercised")
	}
	d := drift(t, in, dir, base)
	if !d.Drift || !d.HeadMoved || d.BranchChanged || d.NewCommitCount != 0 || len(d.NewCommits) != 0 {
		t.Fatalf("drift = %+v", d)
	}
}

func TestDriftFromADetachedSnapshotListsTheCommitsOnHead(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	git(t, dir, "checkout", "-q", "--detach")
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	if base.Branch != "" {
		t.Fatalf("baseline = %+v", base)
	}
	git(t, dir, "commit", "-q", "-m", "detached commit")
	d := drift(t, in, dir, base)
	if !d.Drift || !d.HeadMoved || d.BranchChanged || !d.Detached || d.NewCommitCount != 1 || d.NewCommits[0].Subject != "detached commit" {
		t.Fatalf("drift = %+v", d)
	}
}

func TestDriftListsAtMostAHundredPathsOfEachKindAndCountsThemAll(t *testing.T) {
	t.Parallel()
	dir := driftRepo(t)
	for i := range 101 {
		write(t, filepath.Join(dir, "gen", fmt.Sprintf("f%03d.txt", i)), "x\n")
	}
	in := newInspector(t)
	base := baselineOf(inspect(t, in, dir))
	if err := os.Rename(filepath.Join(dir, "gen"), filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	d := drift(t, in, dir, base)
	if !d.Drift || len(d.Changed) != maxBlockerPaths || d.ChangedCount != 101 || len(d.Untracked) != maxBlockerPaths || d.UntrackedCount != 101 ||
		d.Changed[0] != "gen/f000.txt" || d.Untracked[99] != "moved/f099.txt" {
		t.Fatalf("drift = %d of %d changed, %d of %d untracked", len(d.Changed), d.ChangedCount, len(d.Untracked), d.UntrackedCount)
	}
}
