package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
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
