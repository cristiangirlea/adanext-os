package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const AgentCommitMessage = "Agent changes"

func (w *Workspaces) CheckWorktreePath(project, attempt, runID string) string {
	return filepath.Join(w.Root, project, "checks", attempt, runID)
}

func (w *Workspaces) safeConfigFlags() []string {
	return []string{
		"core.hooksPath=" + filepath.ToSlash(w.Inspector.Git.hooks),
		"core.fsmonitor=false",
		"commit.gpgsign=false",
		"tag.gpgsign=false",
	}
}

func allowedCloneConfigKey(key string) bool {
	switch key {
	case "core.repositoryformatversion", "core.bare", "core.hookspath", "core.fsmonitor",
		"core.excludesfile", "core.attributesfile", "extensions.objectformat",
		"gc.auto", "maintenance.auto":
		return true
	}
	for _, k := range mirrorKeys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

func (w *Workspaces) verifyCloneConfig(repo, admin string) error {
	cfgData, _, err := readUnder(repo, filepath.Join(repo, "config"), maxFileSize)
	if err != nil {
		return mismatch("the clone config cannot be read")
	}
	expected, _, err := readUnder(repo, filepath.Join(repo, "ada-config"), maxFileSize)
	if err != nil || !bytes.Equal(cfgData, expected) {
		return mismatch("the clone config differs from the copy Ada wrote")
	}
	if admin != "" {
		adminExpected, _, err := readUnder(admin, filepath.Join(admin, "ada-config"), maxFileSize)
		if err != nil || !bytes.Equal(cfgData, adminExpected) {
			return mismatch("the clone config differs from the copy Ada wrote")
		}
		for _, name := range []string{"config", "config.worktree"} {
			if _, err := os.Lstat(filepath.Join(admin, name)); !errors.Is(err, fs.ErrNotExist) {
				return mismatch("the worktree git directory holds %s", name)
			}
		}
	}
	parsed, err := parseConfig(cfgData)
	if err != nil {
		return mismatch("the clone config is invalid")
	}
	if hooks, _ := parsed.get("core.hookspath"); hooks != filepath.ToSlash(w.Inspector.Git.hooks) {
		return mismatch("the clone config core.hooksPath was changed")
	}
	if fsm, _ := parsed.get("core.fsmonitor"); fsm != "false" {
		return mismatch("the clone config core.fsmonitor was changed")
	}
	for _, entry := range parsed.entries {
		key := entry.section + "." + entry.name
		if entry.subsection != "" || !allowedCloneConfigKey(key) {
			return mismatch("the clone config holds unexpected key %s", key)
		}
	}
	return nil
}

// CommitAgentChanges verifies the attempt's worktree, clone config, and branch
// ancestry from the imported-changes commit, commits any agent changes on
// ada/<task> with Ada as author and committer, and returns the branch tip commit.
func (w *Workspaces) CommitAgentChanges(ctx context.Context, m Manifest, hash string) (string, error) {
	switch {
	case m.Version != ManifestVersion:
		return "", mismatch("manifest version %d", m.Version)
	case !safeName(m.Project) || !safeName(m.Task) || !safeName(m.Attempt) || m.Path != w.WorktreePath(m.Project, m.Attempt):
		return "", mismatch("the worktree path is not the attempt's")
	}
	defer w.lock(m.Project)()

	info, err := os.Lstat(m.Path)
	if err != nil || !info.IsDir() || isLink(m.Path, info) {
		return "", mismatch("the worktree is missing")
	}
	repo := w.repo(m.Project)
	admin := w.adminDir(m.Project, m.Attempt)
	if !pointsTo(m.Path, admin) {
		return "", mismatch("the worktree does not use its git directory")
	}
	if err := w.verifyCloneConfig(repo, admin); err != nil {
		return "", err
	}
	stored, _, err := readUnder(admin, filepath.Join(admin, manifestFile), maxFileSize)
	if err != nil {
		return "", mismatch("the manifest copy is missing")
	}
	if m.Head == "" {
		var copied Manifest
		if err := json.Unmarshal(stored, &copied); err != nil {
			return "", mismatch("the manifest copy cannot be read")
		}
		m.Head = copied.Head
	}
	data := m.Encode()
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return "", mismatch("the workspace row does not match its manifest hash")
	}
	if !bytes.Equal(stored, data) {
		return "", mismatch("the manifest copy differs from the workspace row")
	}

	head, branch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || branch != m.Branch || head == "" {
		return "", mismatch("the worktree is not on %s", m.Branch)
	}
	g := w.Inspector.Git
	flags := w.safeConfigFlags()
	if head != m.Head {
		cmd := g.commandWith(ctx, flags, repo, "", "merge-base", "--is-ancestor", m.Head, head)
		if _, err := output(cmd, "merge-base"); err != nil {
			return "", mismatch("the branch %s no longer descends from the imported-changes commit", m.Branch)
		}
	}

	addCmd := g.commandWith(ctx, flags, admin, m.Path, "add", "-A", "--")
	addCmd.Dir = m.Path
	if _, err := output(addCmd, "add"); err != nil {
		return "", mismatch("stage agent changes: %v", err)
	}

	diffCmd := g.commandWith(ctx, flags, admin, m.Path, "diff", "--cached", "--quiet")
	diffCmd.Dir = m.Path
	dirty := false
	if _, err := output(diffCmd, "diff"); err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == 1 {
			dirty = true
		} else {
			return "", mismatch("diff staged agent changes: %v", err)
		}
	}

	if dirty || head != m.Head {
		args := []string{"commit", "-q", "--no-verify", "--no-gpg-sign", "-m", AgentCommitMessage}
		if !dirty {
			args = append(args, "--allow-empty")
		}
		commitCmd := g.commandWith(ctx, flags, admin, m.Path, args...)
		commitCmd.Dir = m.Path
		commitCmd.Env = append(commitCmd.Env,
			"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@adanext.invalid",
			"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@adanext.invalid",
		)
		if _, err := output(commitCmd, "commit"); err != nil {
			return "", mismatch("commit agent changes: %v", err)
		}
	}

	newHead, newBranch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || newBranch != m.Branch || newHead == "" {
		return "", mismatch("the worktree left %s after commit", m.Branch)
	}
	return newHead, nil
}

// PrepareCheckWorktree creates a fresh detached worktree at commit under
// <Root>/<project>/checks/<attempt>/<runID>.
func (w *Workspaces) PrepareCheckWorktree(ctx context.Context, project, attempt, runID, commit string) (string, string, error) {
	for _, id := range []string{project, attempt, runID} {
		if !safeName(id) {
			return "", "", fmt.Errorf("identifier %q cannot name a check worktree", id)
		}
	}
	if (len(commit) != 40 && len(commit) != 64) || !isHex(commit) {
		return "", "", fmt.Errorf("commit %q is not an object ID", commit)
	}
	defer w.lock(project)()

	repo := w.repo(project)
	if err := w.verifyCloneConfig(repo, ""); err != nil {
		return "", "", err
	}
	adminName := runID
	_ = w.removeCheckWorktreeLocked(project, attempt, runID, adminName)

	wt := w.CheckWorktreePath(project, attempt, runID)
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return "", "", err
	}
	g := w.Inspector.Git
	cmd := g.commandWith(ctx, w.safeConfigFlags(), repo, "", "worktree", "add", "-q", "--detach", "--lock", "--reason", "Ada check "+runID, wt, commit)
	if _, err := output(cmd, "worktree"); err != nil {
		_ = w.removeCheckWorktreeLocked(project, attempt, runID, adminName)
		return "", "", err
	}
	adminDir := filepath.Join(repo, "worktrees", adminName)
	if !pointsTo(wt, adminDir) {
		_ = w.removeCheckWorktreeLocked(project, attempt, runID, adminName)
		return "", "", fmt.Errorf("check worktree %s does not use %s", wt, adminDir)
	}
	return wt, adminName, nil
}

// CheckTreeUnchanged reports whether the tracked files of a check's worktree
// still match commit.
func (w *Workspaces) CheckTreeUnchanged(ctx context.Context, project, attempt, runID, adminName, commit string) (bool, error) {
	for _, id := range []string{project, attempt, runID} {
		if !safeName(id) {
			return false, fmt.Errorf("invalid check worktree identifier %q", id)
		}
	}
	if adminName == "" {
		adminName = runID
	}
	if !safeName(adminName) {
		return false, fmt.Errorf("invalid check admin identifier %q", adminName)
	}
	defer w.lock(project)()

	wt := w.CheckWorktreePath(project, attempt, runID)
	info, err := os.Lstat(wt)
	if err != nil || !info.IsDir() || isLink(wt, info) {
		return false, nil
	}
	repo := w.repo(project)
	adminDir := filepath.Join(repo, "worktrees", adminName)
	if !pointsTo(wt, adminDir) {
		return false, nil
	}
	if err := w.verifyCloneConfig(repo, ""); err != nil {
		return false, nil
	}
	for _, name := range []string{"config", "config.worktree"} {
		if _, err := os.Lstat(filepath.Join(adminDir, name)); !errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	head, branch, err := resolveHead(layout{gitDir: adminDir, commonDir: repo}, len(commit)/2)
	if err != nil || branch != "" || head != commit {
		return false, nil
	}
	g := w.Inspector.Git
	flags := w.safeConfigFlags()
	statusCmd := g.commandWith(ctx, flags, adminDir, wt, "status", "--porcelain=v1", "-z", "--untracked-files=no")
	statusCmd.Dir = wt
	out, err := output(statusCmd, "status")
	if err != nil || len(out) != 0 {
		return false, nil
	}
	for _, args := range [][]string{
		{"diff", "--quiet", commit, "--"},
		{"diff", "--cached", "--quiet", commit, "--"},
	} {
		cmd := g.commandWith(ctx, flags, adminDir, wt, args...)
		cmd.Dir = wt
		if _, err := output(cmd, "diff"); err != nil {
			return false, nil
		}
	}
	return true, nil
}

// RemoveCheckWorktree removes a check's worktree built from IDs without
// following any symlink or reparse point, and then removes its recorded admin
// directory under the clone's worktrees/, never calling git worktree prune.
func (w *Workspaces) RemoveCheckWorktree(project, attempt, runID, adminName string) error {
	for _, id := range []string{project, attempt, runID} {
		if !safeName(id) {
			return fmt.Errorf("identifier %q cannot name a check worktree", id)
		}
	}
	if adminName == "" {
		adminName = runID
	}
	if !safeName(adminName) {
		return fmt.Errorf("identifier %q cannot name a check admin directory", adminName)
	}
	defer w.lock(project)()
	return w.removeCheckWorktreeLocked(project, attempt, runID, adminName)
}

func (w *Workspaces) removeCheckWorktreeLocked(project, attempt, runID, adminName string) error {
	wt := w.CheckWorktreePath(project, attempt, runID)
	wtErr := removeTreeNoFollow(wt)
	_ = os.Remove(filepath.Dir(wt))
	_ = os.Remove(filepath.Dir(filepath.Dir(wt)))
	adminDir := filepath.Join(w.repo(project), "worktrees", adminName)
	adminErr := removeTreeNoFollow(adminDir)
	return errors.Join(wtErr, adminErr)
}

func removeTreeNoFollow(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if isLink(path, info) || !info.IsDir() {
		_ = os.Chmod(path, 0o600)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		if err := removeTreeNoFollow(child); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	_ = os.Chmod(path, 0o700)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ResolveWorkdirNoFollow resolves rel inside worktree using no-follow directory
// handles, refusing any symlink or reparse point on the path.
func ResolveWorkdirNoFollow(worktree, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if rel != "." {
		if strings.ContainsAny(rel, "\x00\\:") || strings.HasPrefix(rel, "/") || strings.HasSuffix(rel, "/") {
			return "", fmt.Errorf("invalid check workdir %q", rel)
		}
		for _, p := range strings.Split(rel, "/") {
			if p == "" || p == "." || p == ".." || strings.EqualFold(p, ".git") {
				return "", fmt.Errorf("invalid check workdir %q", rel)
			}
		}
	}
	if err := openDirNoFollow(worktree, rel); err != nil {
		return "", err
	}
	if rel == "." {
		return worktree, nil
	}
	return filepath.Join(worktree, filepath.FromSlash(rel)), nil
}

// BranchTip reads the current commit of refs/heads/ada/<task> in the project's clone.
func (w *Workspaces) BranchTip(project, task string) string {
	if !safeName(project) || !safeName(task) {
		return ""
	}
	repo := w.repo(project)
	ref := "refs/heads/ada/" + task
	if data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(ref))); err == nil {
		if s := strings.TrimSpace(string(data)); (len(s) == 40 || len(s) == 64) && isHex(s) {
			return s
		}
	}
	if data, err := os.ReadFile(filepath.Join(repo, "packed-refs")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line[0] == '#' || line[0] == '^' {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref && isHex(fields[0]) {
				return fields[0]
			}
		}
	}
	return ""
}
