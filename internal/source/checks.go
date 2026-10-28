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

// ErrNoAttributeCopies is a clone holding an attributes file Ada kept no copy of: one synced before
// Ada kept them. It is refused until the clone's next preparation, and is not a mismatch, so an
// ended attempt in it is not moved to recovery.
var ErrNoAttributeCopies = errors.New("the clone predates Ada's copies of its attributes files")

// attributeFile is an attributes file git reads in the clone, and Ada's copy of it outside.
type attributeFile struct{ inClone, copy string }

// attributeCopiesMark marks a project whose attribute copies Ada keeps: a clone synced before it
// has none, and its attributes files are refused but not taken as a mismatch.
func (w *Workspaces) attributeCopiesMark(project string) string {
	return filepath.Join(w.Root, project, "ada-attribute-copies")
}

// attributeFiles are the clone's info/attributes and the copy of the user's attributes file.
func (w *Workspaces) attributeFiles(project string) []attributeFile {
	dir := filepath.Join(w.Root, project)
	return []attributeFile{
		{filepath.Join("info", "attributes"), filepath.Join(dir, "ada-info-attributes")},
		{"ada-attributes", filepath.Join(dir, "ada-attributes")},
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

// verifyCloneConfig checks Ada's hooks directory, the clone config against the
// copy Ada wrote outside the clone, and that copy against want, its SHA-256
// from SQLite; with admin, also the worktree's git directory: no config of its
// own, and the clone as its common dir.
func (w *Workspaces) verifyCloneConfig(project, want, admin string) error {
	if err := w.Inspector.Git.checkHooks(); err != nil {
		return err
	}
	repo := w.repo(project)
	cfgData, _, err := readUnder(repo, filepath.Join(repo, "config"), maxFileSize)
	if err != nil {
		return mismatch("the clone config cannot be read")
	}
	expected, _, err := readUnder(w.Root, w.configCopy(project), maxFileSize)
	sum := sha256.Sum256(expected)
	if err != nil || want == "" || hex.EncodeToString(sum[:]) != want || !bytes.Equal(cfgData, expected) {
		return mismatch("the clone config differs from the copy Ada wrote")
	}
	if admin != "" {
		if !sharesCommonDir(admin, repo) {
			return mismatch("the worktree git directory names another common dir")
		}
		for _, name := range []string{"config", "config.worktree"} {
			if _, err := os.Lstat(filepath.Join(admin, name)); !errors.Is(err, fs.ErrNotExist) {
				return mismatch("the worktree git directory holds %s", name)
			}
		}
	}
	_, markErr := os.Lstat(w.attributeCopiesMark(project))
	for _, a := range w.attributeFiles(project) {
		got, _, gotErr := readUnder(repo, filepath.Join(repo, a.inClone), maxFileSize)
		kept, _, keptErr := readUnder(w.Root, a.copy, maxFileSize)
		switch {
		case errors.Is(gotErr, fs.ErrNotExist) && errors.Is(keptErr, fs.ErrNotExist):
		case errors.Is(markErr, fs.ErrNotExist) && gotErr == nil && errors.Is(keptErr, fs.ErrNotExist):
			return fmt.Errorf("%s: %w", filepath.ToSlash(a.inClone), ErrNoAttributeCopies)
		case gotErr != nil || keptErr != nil || !bytes.Equal(got, kept):
			return mismatch("the clone's %s differs from what Ada wrote", filepath.ToSlash(a.inClone))
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
// ada/<task> with Ada as author and committer, and returns the branch tip
// commit and the imported-changes commit it verified.
func (w *Workspaces) CommitAgentChanges(ctx context.Context, m Manifest, hash string) (tested, imported string, err error) {
	switch {
	case m.Version != ManifestVersion:
		return "", "", mismatch("manifest version %d", m.Version)
	case !safeName(m.Project) || !safeName(m.Task) || !safeName(m.Attempt) || m.Path != w.WorktreePath(m.Project, m.Attempt):
		return "", "", mismatch("the worktree path is not the attempt's")
	}
	defer w.lock(m.Project)()

	info, err := os.Lstat(m.Path)
	if err != nil || !info.IsDir() || isLink(m.Path, info) {
		return "", "", mismatch("the worktree is missing")
	}
	repo := w.repo(m.Project)
	admin := w.adminDir(m.Project, m.Attempt)
	if !pointsTo(m.Path, admin) {
		return "", "", mismatch("the worktree does not use its git directory")
	}
	if err := w.verifyCloneConfig(m.Project, m.ConfigSHA256, admin); err != nil {
		return "", "", err
	}
	stored, _, err := readUnder(admin, filepath.Join(admin, manifestFile), maxFileSize)
	if err != nil {
		return "", "", mismatch("the manifest copy is missing")
	}
	if m.Head == "" {
		var copied Manifest
		if err := json.Unmarshal(stored, &copied); err != nil {
			return "", "", mismatch("the manifest copy cannot be read")
		}
		m.Head = copied.Head
	}
	data := m.Encode()
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return "", "", mismatch("the workspace row does not match its manifest hash")
	}
	if !bytes.Equal(stored, data) {
		return "", "", mismatch("the manifest copy differs from the workspace row")
	}

	head, branch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || branch != m.Branch || head == "" {
		return "", "", mismatch("the worktree is not on %s", m.Branch)
	}
	g := w.Inspector.Git
	flags := w.safeConfigFlags()
	if head != m.Head {
		cmd := g.commandWith(ctx, flags, repo, "", "merge-base", "--is-ancestor", m.Head, head)
		if _, err := output(cmd, "merge-base"); err != nil {
			return "", "", mismatch("the branch %s no longer descends from the imported-changes commit", m.Branch)
		}
	}

	// Staged through a fresh index read from head: the agent's own index could
	// mark a changed file assume-unchanged or skip-worktree.
	scratch, err := os.MkdirTemp("", "ada-index-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(scratch)
	inIndex := func(args ...string) *exec.Cmd {
		cmd := g.worktreeCommand(ctx, flags, repo, admin, m.Path, args...)
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
		return cmd
	}
	if _, err := output(inIndex("read-tree", head), "read-tree"); err != nil {
		return "", "", mismatch("read the branch head: %v", err)
	}
	if _, err := output(inIndex("add", "-A", "--"), "add"); err != nil {
		return "", "", mismatch("stage agent changes: %v", err)
	}

	diffCmd := inIndex("diff", "--cached", "--quiet")
	dirty := false
	if _, err := output(diffCmd, "diff"); err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == 1 {
			dirty = true
		} else {
			return "", "", mismatch("diff staged agent changes: %v", err)
		}
	}

	if dirty || head != m.Head {
		args := []string{"commit", "-q", "--no-verify", "--no-gpg-sign", "-m", AgentCommitMessage}
		if !dirty {
			args = append(args, "--allow-empty")
		}
		commitCmd := inIndex(args...)
		commitCmd.Env = append(commitCmd.Env,
			"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@adanext.invalid",
			"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@adanext.invalid",
		)
		if _, err := output(commitCmd, "commit"); err != nil {
			return "", "", mismatch("commit agent changes: %v", err)
		}
	}

	newHead, newBranch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || newBranch != m.Branch || newHead == "" {
		return "", "", mismatch("the worktree left %s after commit", m.Branch)
	}
	return newHead, m.Head, nil
}

// PrepareCheckWorktree creates a fresh detached worktree at commit under
// <Root>/<project>/checks/<attempt>/<runID>.
func (w *Workspaces) PrepareCheckWorktree(ctx context.Context, project, attempt, runID, commit, configSHA256 string) (string, string, error) {
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
	if err := w.verifyCloneConfig(project, configSHA256, ""); err != nil {
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
	if !pointsTo(wt, adminDir) || !sharesCommonDir(adminDir, repo) {
		_ = w.removeCheckWorktreeLocked(project, attempt, runID, adminName)
		return "", "", fmt.Errorf("check worktree %s does not use %s", wt, adminDir)
	}
	return wt, adminName, nil
}

// CheckTreeUnchanged reports whether the tracked files of a check's worktree
// still match commit. A worktree, git directory or config the check moved or
// changed reads changed; a git call that fails for another reason is an error.
func (w *Workspaces) CheckTreeUnchanged(ctx context.Context, project, attempt, runID, adminName, commit, configSHA256 string) (bool, error) {
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
	if err := w.verifyCloneConfig(project, configSHA256, adminDir); err != nil {
		return false, nil
	}
	head, branch, err := resolveHead(layout{gitDir: adminDir, commonDir: repo}, len(commit)/2)
	if err != nil || branch != "" || head != commit {
		return false, nil
	}
	// A fresh index read from commit: the check's own index could mark a
	// changed file assume-unchanged or skip-worktree.
	scratch, err := os.MkdirTemp("", "ada-index-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(scratch)
	// Only diff's exit 1 says the tree changed; any other failure is Ada's own
	// and says nothing about the tree.
	g := w.Inspector.Git
	flags := w.safeConfigFlags()
	for _, args := range [][]string{{"read-tree", commit}, {"diff", "--quiet", commit, "--"}} {
		cmd := g.worktreeCommand(ctx, flags, repo, adminDir, wt, args...)
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
		if _, err := output(cmd, args[0]); err != nil {
			if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == 1 && args[0] == "diff" {
				return false, nil
			}
			return false, err
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
		// Chmod follows a link, so only a real file loses a read-only bit first.
		if !isLink(path, info) && info.Mode().IsRegular() {
			_ = os.Chmod(path, 0o600)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	// A real directory, so chmod changes it alone: a read-only one must open and lose entries.
	_ = os.Chmod(path, 0o700)
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
	// Read without following a link anywhere under the state root; a loose ref
	// that is not a commit id leaves the tip unknown.
	data, _, err := readUnder(w.Root, filepath.Join(repo, filepath.FromSlash(ref)), maxFileSize)
	if err == nil {
		if s := strings.TrimSpace(string(data)); (len(s) == 40 || len(s) == 64) && isHex(s) {
			return s
		}
		return ""
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if data, _, err := readUnder(w.Root, filepath.Join(repo, "packed-refs"), maxFileSize); err == nil {
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
