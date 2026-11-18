package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// onceSeen runs swap the first time a removal has seen the directory rel and is about to
// open it, and reports afterwards whether it ran.
func onceSeen(t *testing.T, w *Workspaces, rel string, swap func(target string)) (ran func() bool) {
	t.Helper()
	target := filepath.Join(w.Root, filepath.FromSlash(rel))
	var once sync.Once
	var did atomic.Bool
	w.beforeEnter = func(path string) {
		if path == target {
			once.Do(func() { swap(target); did.Store(true) })
		}
	}
	return did.Load
}

// linkOutside moves target aside and leaves in its place a link to a directory outside
// that holds a marker at behind.
func linkOutside(t *testing.T, behind string) (swap func(target string), marker string) {
	t.Helper()
	outside := tempDir(t)
	marker = filepath.Join(outside, filepath.FromSlash(behind), "marker.txt")
	write(t, marker, "keep me\n")
	return func(target string) {
		if err := os.Rename(target, target+"-moved"); err != nil {
			t.Error(err)
			return
		}
		makeLink(t, outside, target)
	}, marker
}

func refusedAsMismatch(t *testing.T, err error, ran func() bool, link string) {
	t.Helper()
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("removal after the swap: %v, want a mismatch", err)
	}
	if !ran() {
		t.Fatal("the swap never ran, so the mismatch has another cause")
	}
	if link != "" {
		if info, err := os.Lstat(link); err != nil || !isLink(link, info) {
			t.Fatalf("the link was not left in place: %v", err)
		}
	}
}

var worktreeSwaps = map[string]string{
	"p":                            "worktrees/a1",
	"p/worktrees":                  "a1",
	"p/worktrees/a1":               ".",
	"p/worktrees/a1/inner":         ".",
	"p/repo.git":                   "worktrees/a1",
	"p/repo.git/worktrees":         "a1",
	"p/repo.git/worktrees/a1":      ".",
	"p/repo.git/worktrees/a1/logs": ".",
}

var checkSwaps = map[string]string{
	"p":                              "checks/a1/run1",
	"p/checks":                       "a1/run1",
	"p/checks/a1":                    "run1",
	"p/checks/a1/run1":               ".",
	"p/checks/a1/run1/inner":         ".",
	"p/repo.git":                     "worktrees/run1",
	"p/repo.git/worktrees":           "run1",
	"p/repo.git/worktrees/run1":      ".",
	"p/repo.git/worktrees/run1/logs": ".",
}

func worktreeTree(t *testing.T, w *Workspaces) {
	t.Helper()
	write(t, filepath.Join(w.Root, "p", "worktrees", "a1", "inner", "file.txt"), "x\n")
	write(t, filepath.Join(w.Root, "p", "repo.git", "worktrees", "a1", "logs", "HEAD"), "x\n")
}

func checkTree(t *testing.T, w *Workspaces) {
	t.Helper()
	write(t, filepath.Join(w.Root, "p", "checks", "a1", "run1", "inner", "file.txt"), "x\n")
	write(t, filepath.Join(w.Root, "p", "repo.git", "worktrees", "run1", "logs", "HEAD"), "x\n")
}

func TestWorktreeRemovalDoesNotFollowALinkSwappedInForADirectoryItHasSeen(t *testing.T) {
	t.Parallel()
	for rel, behind := range worktreeSwaps {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			w := newWorkspaces(t)
			worktreeTree(t, w)
			swap, marker := linkOutside(t, behind)
			ran := onceSeen(t, w, rel, swap)
			refusedAsMismatch(t, w.RemoveWorktree("p", "a1"), ran, filepath.Join(w.Root, filepath.FromSlash(rel)))
			keptMarker(t, marker)
		})
	}
}

func TestCheckWorktreeRemovalDoesNotFollowALinkSwappedInForADirectoryItHasSeen(t *testing.T) {
	t.Parallel()
	for rel, behind := range checkSwaps {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			w := newWorkspaces(t)
			checkTree(t, w)
			swap, marker := linkOutside(t, behind)
			ran := onceSeen(t, w, rel, swap)
			refusedAsMismatch(t, w.RemoveCheckWorktree("p", "a1", "run1", "run1"), ran, filepath.Join(w.Root, filepath.FromSlash(rel)))
			keptMarker(t, marker)
		})
	}
}

// On Unix the opened directory is then the one seen, reached through a link: still a mismatch.
func TestRemovalRefusesALinkToTheDirectoryItHasSeenMovedAside(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"p/worktrees", "p/worktrees/a1", "p/worktrees/a1/inner", "p/repo.git/worktrees"} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			w := newWorkspaces(t)
			worktreeTree(t, w)
			ran := onceSeen(t, w, rel, func(target string) {
				if err := os.Rename(target, target+"-moved"); err != nil {
					t.Error(err)
					return
				}
				if runtime.GOOS == "windows" {
					makeLink(t, target+"-moved", target)
					return
				}
				// Relative, so it stays inside every directory above it.
				if err := os.Symlink(filepath.Base(target)+"-moved", target); err != nil {
					t.Error(err)
				}
			})
			link := filepath.Join(w.Root, filepath.FromSlash(rel))
			refusedAsMismatch(t, w.RemoveWorktree("p", "a1"), ran, link)
			if entries, err := os.ReadDir(link + "-moved"); err != nil || len(entries) == 0 {
				t.Fatalf("the directory moved aside lost its entries: %v %v", entries, err)
			}
		})
	}
}

func TestRemovalRefusesAnotherRealDirectoryPutInPlaceOfTheOneItHasSeen(t *testing.T) {
	t.Parallel()
	for rel, behind := range map[string]string{"p": "worktrees/a1", "p/worktrees": "a1", "p/worktrees/a1": "."} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			w := newWorkspaces(t)
			worktreeTree(t, w)
			var marker string
			ran := onceSeen(t, w, rel, func(target string) {
				if err := os.Rename(target, target+"-moved"); err != nil {
					t.Error(err)
					return
				}
				marker = filepath.Join(target, filepath.FromSlash(behind), "marker.txt")
				write(t, marker, "keep me\n")
			})
			refusedAsMismatch(t, w.RemoveWorktree("p", "a1"), ran, "")
			keptMarker(t, marker)
		})
	}
}

func TestRemovalRemovesReadOnlyFilesAndDirectories(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	worktreeTree(t, w)
	wt := filepath.Join(w.Root, "p", "worktrees", "a1")
	file := filepath.Join(wt, "inner", "file.txt")
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, mode := range []fs.FileMode{0o555, 0o400, 0o000} {
			dir := filepath.Join(wt, "sealed", mode.String())
			write(t, filepath.Join(dir, "file.txt"), "x\n")
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
		}
	}
	if err := w.RemoveWorktree("p", "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the worktree remains: %v", err)
	}
}

func TestCheckWorktreeRemovalPrunesItsOwnDirectoriesAndNeverTheClones(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	checkTree(t, w)
	if err := w.RemoveCheckWorktree("p", "a1", "run1", "run1"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"p/checks", "p/repo.git/worktrees/run1"} {
		if _, err := os.Lstat(filepath.Join(w.Root, filepath.FromSlash(gone))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s remains: %v", gone, err)
		}
	}
	for _, kept := range []string{"p/repo.git", "p/repo.git/worktrees"} {
		if info, err := os.Lstat(filepath.Join(w.Root, filepath.FromSlash(kept))); err != nil || !info.IsDir() {
			t.Fatalf("%s, now empty, was removed: %v", kept, err)
		}
	}
}

func TestRemovalNamesTheDirectoryThatIsNotARealOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rel, attempt, check string
	}{
		{"p", "the project's state directory is not a real directory", "p is not a real directory"},
		{"p/worktrees", "the worktrees directory is not a real directory", ""},
		{"p/checks", "", "p/checks is not a real directory"},
		{"p/checks/a1", "", "p/checks/a1 is not a real directory"},
		{"p/repo.git", "the clone's worktrees directory is not a real directory", "p/repo.git is not a real directory"},
		{"p/repo.git/worktrees", "the clone's worktrees directory is not a real directory", "p/repo.git/worktrees is not a real directory"},
	} {
		t.Run(c.rel, func(t *testing.T) {
			t.Parallel()
			w := newWorkspaces(t)
			worktreeTree(t, w)
			checkTree(t, w)
			link := filepath.Join(w.Root, filepath.FromSlash(c.rel))
			if err := os.Rename(link, link+"-moved"); err != nil {
				t.Fatal(err)
			}
			makeLink(t, tempDir(t), link)
			reason := func(err error) string {
				var mm *MismatchError
				if !errors.As(err, &mm) {
					t.Fatalf("%v, want a mismatch", err)
				}
				return mm.Reason
			}
			if c.attempt != "" {
				if got := reason(w.RemoveWorktree("p", "a1")); got != c.attempt {
					t.Fatalf("the attempt's removal says %q, want %q", got, c.attempt)
				}
			}
			if c.check != "" {
				if got := reason(w.RemoveCheckWorktree("p", "a1", "run1", "run1")); got != c.check {
					t.Fatalf("the check's removal says %q, want %q", got, c.check)
				}
			}
		})
	}
}

func TestRemovalTakesSiblingsInOrderAndGoesOnPastOneItRefuses(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	wt := filepath.Join(w.Root, "p", "worktrees", "a1")
	for _, name := range []string{"c", "a", "d", "b"} {
		write(t, filepath.Join(wt, name, "file.txt"), "x\n")
	}
	outside := tempDir(t)
	var seen []string
	w.beforeEnter = func(path string) {
		if filepath.Dir(path) != wt {
			return
		}
		name := filepath.Base(path)
		seen = append(seen, name)
		if name == "b" || name == "d" {
			if err := os.Rename(path, filepath.Join(outside, name+"-moved")); err != nil {
				t.Error(err)
				return
			}
			makeLink(t, outside, path)
		}
	}
	var mm *MismatchError
	if err := w.RemoveWorktree("p", "a1"); !errors.As(err, &mm) || mm.Reason != "b changed while it was being removed" {
		t.Fatalf("removal with b and d swapped for links: %v, want the mismatch at b", err)
	}
	if !slices.Equal(seen, []string{"a", "b", "c", "d"}) {
		t.Fatalf("the siblings were taken as %v, want them in order", seen)
	}
	for _, gone := range []string{"a", "c"} {
		if _, err := os.Lstat(filepath.Join(wt, gone)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s, with nothing swapped, remains: %v", gone, err)
		}
	}
	for _, link := range []string{"b", "d"} {
		if info, err := os.Lstat(filepath.Join(wt, link)); err != nil || !isLink(filepath.Join(wt, link), info) {
			t.Fatalf("the link at %s was not left in place: %v", link, err)
		}
	}
}
