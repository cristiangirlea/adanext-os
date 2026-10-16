package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAShallowRepositoryInspectsWithItsHistory(t *testing.T) {
	t.Parallel()
	origin := newRepo(t, nil)
	for _, n := range []string{"two", "three"} {
		write(t, filepath.Join(origin, n+".txt"), n+"\n")
		git(t, origin, "add", "-A")
		git(t, origin, "commit", "-q", "-m", n)
	}
	parent := tempDir(t)
	git(t, parent, "clone", "-q", "--depth", "1", fileURL(origin), "shallow")
	dir := filepath.Join(parent, "shallow")
	if _, err := os.Stat(filepath.Join(dir, ".git", "shallow")); err != nil {
		t.Fatalf("the clone is not shallow: %v", err)
	}
	r := inspect(t, newInspector(t), dir)
	if len(r.Blockers) != 0 {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
	if len(r.History) != 1 || r.History[0].Subject != "three" || r.History[0].Commit != r.Identity.Head {
		t.Fatalf("history = %+v", r.History)
	}
}

func TestAPathIgnoredOnlyByTheGlobalExcludesFileIsIgnored(t *testing.T) {
	t.Parallel()
	for _, keySet := range []bool{true, false} {
		dir := newRepo(t, nil)
		write(t, filepath.Join(dir, "secret.txt"), "s\n")
		write(t, filepath.Join(dir, "keep.txt"), "k\n")
		in := newInspector(t)
		if r := inspect(t, in, dir); !hasPath(r.Status.Untracked, "secret.txt") {
			t.Fatalf("without the global excludes file, untracked = %v", r.Status.Untracked)
		}
		home := tempDir(t)
		in.Config = ConfigFiles{XDG: filepath.Join(home, ".config", "git"), Home: home}
		if keySet {
			// core.excludesFile set in the global config, with ~/ as git expands it.
			global := filepath.Join(home, ".gitconfig")
			write(t, global, "[core]\n\texcludesFile = ~/my-ignore\n")
			write(t, filepath.Join(home, "my-ignore"), "secret.txt\n")
			in.Config.Global = []string{global}
		} else {
			// Unset, git's default under XDG applies.
			write(t, filepath.Join(home, ".config", "git", "ignore"), "secret.txt\n")
		}
		r := inspect(t, in, dir)
		if hasPath(r.Status.Untracked, "secret.txt") || !hasPath(r.Status.Untracked, "keep.txt") {
			t.Fatalf("key set %v: untracked = %v", keySet, r.Status.Untracked)
		}
	}
}

func TestAGlobalExcludesFileThatIsALinkIsFollowedButARepositoryOneIsNot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs a privilege CI does not have")
	}
	home := tempDir(t)
	write(t, filepath.Join(home, "dotfiles", "ignore"), "secret.txt\n")
	link := filepath.Join(home, ".config", "git", "ignore")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../dotfiles/ignore", link); err != nil {
		t.Fatal(err)
	}
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "secret.txt"), "s\n")
	in := newInspector(t)
	in.Config = ConfigFiles{XDG: filepath.Join(home, ".config", "git"), Home: home}
	if r := inspect(t, in, dir); hasPath(r.Status.Untracked, "secret.txt") {
		t.Fatalf("the linked global excludes file was not followed: untracked = %v", r.Status.Untracked)
	}
	other := newRepo(t, nil)
	write(t, filepath.Join(other, "secret.txt"), "s\n")
	git(t, other, "config", "core.excludesFile", filepath.ToSlash(link))
	if r := inspect(t, newInspector(t), other); !hasPath(r.Status.Untracked, "secret.txt") {
		t.Fatalf("the repository config's linked excludes file was followed: untracked = %v", r.Status.Untracked)
	}
}

func TestStagedUnstagedAndUntrackedAreToldApart(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"staged.txt": "1\n", "unstaged.txt": "1\n", "both.txt": "1\n", "gone.txt": "1\n"})
	write(t, filepath.Join(dir, "staged.txt"), "2\n")
	write(t, filepath.Join(dir, "both.txt"), "2\n")
	write(t, filepath.Join(dir, "added.txt"), "new\n")
	git(t, dir, "add", "staged.txt", "both.txt", "added.txt")
	write(t, filepath.Join(dir, "both.txt"), "3\n")
	write(t, filepath.Join(dir, "unstaged.txt"), "2\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "dir", "new.txt"), "u\n")
	before := hashTree(t, dir)
	r := inspect(t, newInspector(t), dir)
	sameTree(t, before, hashTree(t, dir))
	want := Status{
		Staged:    []Change{{"added.txt", "A"}, {"both.txt", "M"}, {"staged.txt", "M"}},
		Unstaged:  []Change{{"both.txt", "M"}, {"gone.txt", "D"}, {"unstaged.txt", "M"}},
		Untracked: []string{"dir/new.txt"},
	}
	if !sameChanges(r.Status.Staged, want.Staged) || !sameChanges(r.Status.Unstaged, want.Unstaged) || len(r.Status.Untracked) != 1 || r.Status.Untracked[0] != "dir/new.txt" {
		t.Fatalf("status = %+v, want %+v", r.Status, want)
	}
}

func sameChanges(got, want []Change) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestTheIndexCopyKeepsTheOriginalsModTime(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	index := filepath.Join(dir, ".git", "index")
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second).Add(123456700 * time.Nanosecond)
	if err := os.Chtimes(index, old, old); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	mirror := tempDir(t)
	if _, err := newInspector(t).inspect(context.Background(), dir, mirror); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Stat(filepath.Join(mirror, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !copied.ModTime().Equal(original.ModTime()) {
		t.Fatalf("index copy mtime %v, original %v", copied.ModTime(), original.ModTime())
	}
}

func TestTheFingerprintFollowsContentNotStatData(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	write(t, filepath.Join(dir, "b.txt"), "b changed\n")
	in := newInspector(t)
	fp := func() string { return inspect(t, in, dir).Fingerprint }
	base := fp()
	if base == "" || fp() != base {
		t.Fatal("the fingerprint is not stable")
	}

	// A git status in the original rewrites only the index's stat data.
	backdate(t, filepath.Join(dir, "a.txt"))
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
	if got := fp(); got != base {
		t.Fatal("a stat-only index rewrite changed the fingerprint")
	}

	write(t, filepath.Join(dir, "b.txt"), "b changed again\n")
	changed := fp()
	if changed == base {
		t.Fatal("editing a changed file kept the fingerprint")
	}
	git(t, dir, "add", "b.txt")
	staged := fp()
	if staged == changed {
		t.Fatal("staging kept the fingerprint")
	}
	git(t, dir, "commit", "-q", "-m", "second")
	if fp() == staged {
		t.Fatal("a new HEAD kept the fingerprint")
	}
}
