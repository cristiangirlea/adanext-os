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
	"testing"
)

// tamperConfig appends text to the clone's config and to every copy Ada keeps of it.
func tamperConfig(t *testing.T, w *Workspaces, m Manifest, text string) {
	t.Helper()
	repo := w.repo(m.Project)
	data, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil {
		t.Fatal(err)
	}
	evil := append(data, text...)
	for _, p := range []string{
		filepath.Join(repo, "config"),
		filepath.Join(repo, "ada-config"),
		filepath.Join(w.adminDir(m.Project, m.Attempt), "ada-config"),
		filepath.Join(w.Root, m.Project, "ada-config"),
	} {
		if err := os.WriteFile(p, evil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitAgentChangesRefusesAConfigRewrittenTogetherWithItsCopy(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	// An allowed key, so only the copy Ada wrote can tell.
	tamperConfig(t, w, m, "[gc]\n\tauto = 1\n")
	row := m
	row.Head = ""
	_, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
	if _, ok := errors.AsType[*MismatchError](err); !ok {
		t.Fatalf("CommitAgentChanges = %v, want a mismatch", err)
	}
	if _, _, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run1", m.Head, m.ConfigSHA256); err == nil {
		t.Fatal("a check worktree was prepared on a rewritten config")
	}
}

func TestCommitAgentChangesRefusesARepointedCommonDirAndRunsNoFilter(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	marker := filepath.Join(tempDir(t), "filter-ran")
	evil := filepath.Join(tempDir(t), "evil.git")
	git(t, tempDir(t), "init", "-q", "--bare", evil)
	f, err := os.OpenFile(filepath.Join(evil, "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[filter \"evil\"]\n\tclean = touch '" + shPath(marker) + "'\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	write(t, filepath.Join(m.Path, ".gitattributes"), "* filter=evil\n")
	write(t, filepath.Join(m.Path, "README.md"), "changed\n")
	admin := w.adminDir(m.Project, m.Attempt)
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(shPath(evil)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	row := m
	row.Head = ""
	_, _, err = w.CommitAgentChanges(context.Background(), row, m.Hash())
	if _, ok := errors.AsType[*MismatchError](err); !ok {
		t.Errorf("CommitAgentChanges = %v, want a mismatch", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the evil common dir's filter ran: %v", err)
	}
}

func TestCheckTreeUnchangedSeesAChangeTheIndexHides(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			w := newWorkspaces(t)
			m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
			wt, admin, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run1", m.Head, m.ConfigSHA256)
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(wt, "README.md"), "changed by a check\n")
			git(t, wt, "update-index", flag, "README.md")
			unchanged, err := w.CheckTreeUnchanged(context.Background(), m.Project, m.Attempt, "run1", admin, m.Head, m.ConfigSHA256)
			if err != nil || unchanged {
				t.Fatalf("a change hidden by %s: unchanged = %v, %v", flag, unchanged, err)
			}
		})
	}
}

func TestBranchTipFollowsNoLinkInTheClone(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	if tip := w.BranchTip(m.Project, m.Task); tip != m.Head {
		t.Fatalf("tip = %q, want %s", tip, m.Head)
	}
	outside := tempDir(t)
	write(t, filepath.Join(outside, m.Task), strings.Repeat("e", 40)+"\n")
	heads := filepath.Join(w.repo(m.Project), "refs", "heads", "ada")
	if err := os.RemoveAll(heads); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", heads, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v: %s", err, out)
		}
	} else if err := os.Symlink(outside, heads); err != nil {
		t.Fatal(err)
	}
	if tip := w.BranchTip(m.Project, m.Task); tip != "" {
		t.Fatalf("tip read through a link = %q", tip)
	}
}

func TestCommitAgentChangesTakesAChangeTheAgentsIndexHides(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			w := newWorkspaces(t)
			m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
			write(t, filepath.Join(m.Path, "README.md"), "hidden by the agent\n")
			git(t, m.Path, "update-index", flag, "README.md")
			row := m
			row.Head = ""
			tested, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if got := git(t, m.Path, "show", tested+":README.md"); got != "hidden by the agent" {
				t.Fatalf("Ada's commit holds %q", got)
			}
		})
	}
}

func TestAPlantedOrLinkedHooksDirectoryIsRefusedBeforeAnyGitRuns(t *testing.T) {
	t.Parallel()
	for _, plant := range []string{"hook", "link", "file"} {
		t.Run(plant, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			w := newWorkspaces(t)
			m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
			_, admin, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run0", m.Head, m.ConfigSHA256)
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(m.Path, "README.md"), "agent change\n")
			marker := filepath.Join(tempDir(t), "hook-ran")
			hooks := w.Inspector.Git.hooks
			target := hooks
			switch plant {
			case "file":
				if err := os.Remove(hooks); err != nil {
					t.Fatal(err)
				}
				write(t, hooks, "not a directory\n")
			case "link":
				target = tempDir(t)
				if err := os.Remove(hooks); err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS == "windows" {
					if out, err := exec.Command("cmd", "/c", "mklink", "/J", hooks, target).CombinedOutput(); err != nil {
						t.Fatalf("mklink /J: %v: %s", err, out)
					}
				} else if err := os.Symlink(target, hooks); err != nil {
					t.Fatal(err)
				}
			}
			// Hooks that --no-verify does not skip, on every git call Ada makes
			// here; a link is refused even to an empty directory, and a file
			// is no directory at all.
			for _, name := range []string{"post-index-change", "reference-transaction", "post-commit", "post-checkout"} {
				if plant != "hook" {
					break
				}
				if err := os.WriteFile(filepath.Join(target, name), []byte("#!/bin/sh\necho "+name+" >> '"+shPath(marker)+"'\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			row := m
			row.Head = ""
			if _, _, err := w.CommitAgentChanges(context.Background(), row, m.Hash()); err == nil {
				t.Error("CommitAgentChanges ran with a planted hooks directory")
			}
			if _, _, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run1", m.Head, m.ConfigSHA256); err == nil {
				t.Error("PrepareCheckWorktree ran with a planted hooks directory")
			}
			if _, err := prepareCapture(w, dir, mustCapture(t, w.Inspector, dir), "2"); err == nil {
				t.Error("Prepare synced with a planted hooks directory")
			}
			if unchanged, err := w.CheckTreeUnchanged(context.Background(), m.Project, m.Attempt, "run0", admin, m.Head, m.ConfigSHA256); err != nil || unchanged {
				t.Errorf("CheckTreeUnchanged with a planted hooks directory = %v, %v, want false, nil", unchanged, err)
			}
			if data, err := os.ReadFile(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a hook ran: %q, %v", data, err)
			}
		})
	}
}

func TestCommitAgentChangesReturnsTheImportedCommitItVerified(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	write(t, filepath.Join(m.Path, "README.md"), "agent change\n")
	row := m
	row.Head = ""
	tested, imported, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if imported != m.Head || tested == m.Head {
		t.Fatalf("tested %s, imported %s, want imported %s", tested, imported, m.Head)
	}
}

func TestCheckTreeUnchangedReturnsAdasOwnGitErrors(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	_, admin, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run1", m.Head, m.ConfigSHA256)
	if err != nil {
		t.Fatal(err)
	}
	// git never starts: that says nothing about the tree.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unchanged, err := w.CheckTreeUnchanged(ctx, m.Project, m.Attempt, "run1", admin, m.Head, m.ConfigSHA256)
	if err == nil {
		t.Fatalf("a git call that never ran: unchanged = %v, no error", unchanged)
	}
}

func TestCheckTreeUnchangedReadsARemovedWorktreeAsChanged(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	w := newWorkspaces(t)
	m := mustPrepare(t, w, dir, mustCapture(t, w.Inspector, dir), "1")
	wt, admin, err := w.PrepareCheckWorktree(context.Background(), m.Project, m.Attempt, "run1", m.Head, m.ConfigSHA256)
	if err != nil {
		t.Fatal(err)
	}
	// The check removed its own worktree: a change, not Ada's own error.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	unchanged, err := w.CheckTreeUnchanged(context.Background(), m.Project, m.Attempt, "run1", admin, m.Head, m.ConfigSHA256)
	if err != nil || unchanged {
		t.Fatalf("a removed worktree: unchanged = %v, %v, want false, nil", unchanged, err)
	}
}

func TestWorktreeCommandNamesTheCloneAsItsCommonDir(t *testing.T) {
	t.Parallel()
	g := newInspector(t).Git
	repo, admin, wt := tempDir(t), tempDir(t), tempDir(t)
	cmd := g.worktreeCommand(context.Background(), nil, repo, admin, wt, "status")
	found := 0
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_COMMON_DIR=") {
			found++
			if kv != "GIT_COMMON_DIR="+repo {
				t.Errorf("env holds %s, want GIT_COMMON_DIR=%s", kv, repo)
			}
		}
	}
	if found != 1 || cmd.Dir != wt {
		t.Fatalf("GIT_COMMON_DIR set %d times, dir %s: %v", found, cmd.Dir, cmd.Env)
	}
}
