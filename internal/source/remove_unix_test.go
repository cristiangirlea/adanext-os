//go:build !windows

package source

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRemovalDoesNotWaitOnAPipeSwappedInForADirectoryItHasSeen(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	worktreeTree(t, w)
	pipe := filepath.Join(w.Root, "p", "worktrees", "a1", "inner")
	ran := onceSeen(t, w, "p/worktrees/a1/inner", func(target string) {
		if err := os.Rename(target, target+"-moved"); err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Mkfifo(target, 0o600); err != nil {
			t.Error(err)
		}
	})
	done := make(chan error, 1)
	go func() { done <- w.RemoveWorktree("p", "a1") }()
	select {
	case err := <-done:
		refusedAsMismatch(t, err, ran, "")
	case <-time.After(5 * time.Second):
		// A writer lets an open that waits return, so the test can clean up.
		if f, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("the removal waits on the pipe")
	}
}

// siblingAttempt is another attempt's worktree beside a1's, holding one file.
func siblingAttempt(t *testing.T, w *Workspaces) string {
	t.Helper()
	sibling := filepath.Join(w.Root, "p", "worktrees", "a2")
	write(t, filepath.Join(sibling, "file.txt"), "x\n")
	return sibling
}

// linkToSibling moves target aside and leaves a link to the directory beside it in its place.
func linkToSibling(t *testing.T, sibling string) func(target string) {
	return func(target string) {
		if err := os.Rename(target, target+"-moved"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(filepath.Base(sibling), target); err != nil {
			t.Error(err)
		}
	}
}

func TestRemovalLeavesTheModeOfAnUnreadableDirectoryALinkSwappedInPointsAt(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	worktreeTree(t, w)
	sibling := siblingAttempt(t, w)
	if err := os.Chmod(sibling, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sibling, 0o700) })
	ran := onceSeen(t, w, "p/worktrees/a1", linkToSibling(t, sibling))
	refusedAsMismatch(t, w.RemoveWorktree("p", "a1"), ran, filepath.Join(w.Root, "p", "worktrees", "a1"))
	if info, err := os.Lstat(sibling); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("the other attempt's directory: %v %v, want mode 0000", info.Mode(), err)
	}
}

func TestRemovalRefusesADirectoryOpenedThroughALinkThatWasThenTakenAway(t *testing.T) {
	t.Parallel()
	w := newWorkspaces(t)
	worktreeTree(t, w)
	sibling := siblingAttempt(t, w)
	ran := onceSeen(t, w, "p/worktrees/a1", linkToSibling(t, sibling))
	target := filepath.Join(w.Root, "p", "worktrees", "a1")
	var back atomic.Bool
	w.afterOpen = func(path string) {
		if path != target || back.Swap(true) {
			return
		}
		if err := os.Remove(target); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(target+"-moved", target); err != nil {
			t.Error(err)
		}
	}
	refusedAsMismatch(t, w.RemoveWorktree("p", "a1"), ran, "")
	if !back.Load() {
		t.Fatal("the directory was never put back, so the mismatch has another cause")
	}
	if _, err := os.Lstat(filepath.Join(sibling, "file.txt")); err != nil {
		t.Fatalf("the other attempt's worktree lost its file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "inner", "file.txt")); err != nil {
		t.Fatalf("the worktree put back lost its file: %v", err)
	}
}
