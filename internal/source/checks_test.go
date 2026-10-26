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
	_, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
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
	_, err = w.CommitAgentChanges(context.Background(), row, m.Hash())
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
			tested, err := w.CommitAgentChanges(context.Background(), row, m.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if got := git(t, m.Path, "show", tested+":README.md"); got != "hidden by the agent" {
				t.Fatalf("Ada's commit holds %q", got)
			}
		})
	}
}
