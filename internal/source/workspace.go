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
	"sync"
)

// ManifestVersion is the version of the workspace manifest this code writes;
// version 2 added the task revision and the ownership generation.
const ManifestVersion = 2

const (
	manifestFile  = "ada-manifest.json"
	importMessage = "Imported working changes"
)

// ErrUnbornBase refuses a snapshot taken on a branch with no commit: there is
// no base for the task's branch.
var ErrUnbornBase = errors.New("unborn_base: the snapshot's repository had no commit")

// MismatchError is a workspace that no longer matches its manifest.
type MismatchError struct{ Reason string }

func (e *MismatchError) Error() string { return "workspace manifest mismatch: " + e.Reason }

func mismatch(format string, args ...any) error {
	return &MismatchError{Reason: fmt.Sprintf(format, args...)}
}

// Manifest binds an attempt's worktree to what it was prepared from.
type Manifest struct {
	Version      int    `json:"version"`
	Project      string `json:"project"`
	Task         string `json:"task"`
	TaskRevision int64  `json:"task_revision"`
	Attempt      string `json:"attempt"`
	Generation   int64  `json:"ownership_generation"`
	Snapshot     string `json:"snapshot"`
	BaseCommit   string `json:"base_commit"`
	Branch       string `json:"branch"`
	Path         string `json:"path"`
	Executor     string `json:"executor"`
	Policy       string `json:"policy"`
	Head         string `json:"head"`
	// ConfigSHA256 is the hash of the clone config Ada wrote, kept in SQLite, not in the manifest.
	ConfigSHA256 string `json:"-"`
}

// Encode is the manifest as stored beside the worktree.
func (m Manifest) Encode() []byte {
	data, _ := json.Marshal(m)
	return append(data, '\n')
}

// Hash is the SHA-256 of Encode, as the workspace row records it.
func (m Manifest) Hash() string {
	sum := sha256.Sum256(m.Encode())
	return hex.EncodeToString(sum[:])
}

// Changes is a snapshot's content: its patches and selected untracked files.
type Changes struct {
	Staged, Unstaged []byte
	Untracked        []File
}

// Workspaces keeps Ada's clone of each project, <Root>/<project>/repo.git,
// and the attempts' worktrees, <Root>/<project>/worktrees/<attempt>.
type Workspaces struct {
	Inspector *Inspector
	Root      string
	mu        sync.Mutex
	locks     map[string]*sync.Mutex
	// AfterSync is a test seam run after each sync under the project's lock,
	// telling a clone from a fetch.
	AfterSync func(ctx context.Context, cloned bool)
}

func (w *Workspaces) repo(project string) string {
	return filepath.Join(w.Root, project, "repo.git")
}

// WorktreePath is where an attempt's worktree lives.
func (w *Workspaces) WorktreePath(project, attempt string) string {
	return filepath.Join(w.Root, project, "worktrees", attempt)
}

// adminDir is the worktree's git directory in the clone, named after the
// worktree directory by git worktree add.
func (w *Workspaces) adminDir(project, attempt string) string {
	return filepath.Join(w.repo(project), "worktrees", attempt)
}

// lock takes the project's lock, which one clone, fetch or worktree
// preparation holds at a time, and returns its release.
func (w *Workspaces) lock(project string) func() {
	w.mu.Lock()
	if w.locks == nil {
		w.locks = map[string]*sync.Mutex{}
	}
	l := w.locks[project]
	if l == nil {
		l = &sync.Mutex{}
		w.locks[project] = l
	}
	w.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// safeName is an identifier that can name a directory and a branch component.
func safeName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !alphanumericByte(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func alphanumericByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// Prepare brings Ada's clone of the project up to date from the repository at
// source, creates the task's branch ada/<task> at the base commit and the
// attempt's worktree on it, commits the snapshot's changes there as one
// commit, and writes the manifest into the worktree's git directory. m names
// the project, the task and its revision, the attempt and its ownership
// generation, the snapshot, base commit, executor and policy; the returned
// manifest adds the version, branch, path and prepared HEAD commit.
func (w *Workspaces) Prepare(ctx context.Context, source string, m Manifest, changes Changes) (Manifest, error) {
	for _, id := range []string{m.Project, m.Task, m.Attempt} {
		if !safeName(id) {
			return Manifest{}, fmt.Errorf("identifier %q cannot name a workspace", id)
		}
	}
	if m.BaseCommit == "" {
		return Manifest{}, ErrUnbornBase
	}
	if (len(m.BaseCommit) != 40 && len(m.BaseCommit) != 64) || !isHex(m.BaseCommit) {
		return Manifest{}, fmt.Errorf("base commit %q is not an object ID", m.BaseCommit)
	}
	defer w.lock(m.Project)()
	configHash, err := w.sync(ctx, m.Project, source, m.BaseCommit)
	if err != nil {
		return Manifest{}, fmt.Errorf("update the clone: %w", err)
	}
	g, repo := w.Inspector.Git, w.repo(m.Project)
	if _, err := g.run(ctx, repo, "", "cat-file", "-e", m.BaseCommit+"^{commit}"); err != nil {
		return Manifest{}, fmt.Errorf("the base commit %s is not in the clone: %w", m.BaseCommit, err)
	}
	m.Version, m.Branch, m.Path = ManifestVersion, "ada/"+m.Task, w.WorktreePath(m.Project, m.Attempt)
	if _, err := g.run(ctx, repo, "", "branch", "--no-track", m.Branch, m.BaseCommit); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return Manifest{}, err
	}
	// Locked, so a git gc never prunes the git directory that holds the manifest.
	if _, err := g.run(ctx, repo, "", "worktree", "add", "-q", "--lock", "--reason", "Ada attempt "+m.Attempt, m.Path, m.Branch); err != nil {
		return Manifest{}, err
	}
	admin := w.adminDir(m.Project, m.Attempt)
	if !pointsTo(m.Path, admin) || !sharesCommonDir(admin, repo) {
		return Manifest{}, fmt.Errorf("the worktree at %s does not use %s", m.Path, admin)
	}
	if err := w.commitChanges(ctx, repo, admin, m.Path, changes); err != nil {
		return Manifest{}, err
	}
	head, branch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || branch != m.Branch {
		return Manifest{}, fmt.Errorf("the worktree's HEAD is not on %s: %q, %v", m.Branch, branch, err)
	}
	m.Head, m.ConfigSHA256 = head, configHash
	if err := os.WriteFile(filepath.Join(admin, manifestFile), m.Encode(), 0o600); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// sync clones the project's repository from a freshly built mirror the first
// time, then fetches from one, and gives the clone Ada's config. Neither call
// reads the original's config, and --no-local makes pack-objects copy what
// the mirror reaches through alternates into the clone's own pack. The mirror
// names base as refs/ada/base when it has the commit, so the clone gets it
// however the original's refs have moved since the snapshot. It returns the
// SHA-256 of the config it wrote.
func (w *Workspaces) sync(ctx context.Context, project, source, base string) (string, error) {
	in := w.Inspector
	if err := in.Git.checkHooks(); err != nil {
		return "", err
	}
	dir := filepath.Join(w.Root, project)
	for _, d := range []string{dir, in.MirrorRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	mirror, err := os.MkdirTemp(in.MirrorRoot, "w")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(mirror)
	r, src, err := in.inspectMirror(ctx, source, mirror)
	if err != nil {
		return "", err
	}
	if src == nil {
		return "", &BlockedError{Blockers: r.Blockers}
	}
	// A base the original has pruned gets no ref: the clone may still hold
	// it, else Prepare names it missing.
	_, err = in.Git.run(ctx, mirror, "", "cat-file", "-e", base+"^{commit}")
	hasBase := err == nil
	if hasBase {
		if err := os.MkdirAll(filepath.Join(mirror, "refs", "ada"), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(mirror, "refs", "ada", "base"), []byte(base+"\n"), 0o600); err != nil {
			return "", err
		}
	}
	transport := []string{"protocol.file.allow=always", "transfer.bundleURI=false"}
	repo := w.repo(project)
	_, err = os.Lstat(repo)
	cloned := errors.Is(err, fs.ErrNotExist)
	var configHash string
	if err != nil && !cloned {
		return "", err
	}
	if cloned {
		// A clone a crash interrupted never reached repo.git.
		stale, _ := filepath.Glob(filepath.Join(dir, "clone-*"))
		for _, s := range stale {
			if err := os.RemoveAll(s); err != nil {
				return "", err
			}
		}
		tmp, err := os.MkdirTemp(dir, "clone-")
		if err != nil {
			return "", err
		}
		if _, err := output(in.Git.commandWith(ctx, transport, "", "", "clone", "-q", "--bare", "--no-local", "--no-hardlinks", "--template=", "--", mirror, tmp), "clone"); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
		if err := w.dropAdaBranches(ctx, tmp); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
		if configHash, err = w.writeConfig(project, tmp, mirror, *src); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
		if err := os.Rename(tmp, repo); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
	} else {
		if err := removeLocks(repo); err != nil {
			return "", err
		}
		if configHash, err = w.writeConfig(project, repo, mirror, *src); err != nil {
			return "", err
		}
	}
	branches, err := in.Git.run(ctx, mirror, "", "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return "", err
	}
	// refs/ada/head carries a detached HEAD no branch reaches; ada and ada/*,
	// in any case, are Ada's own names, which the fetch never takes and
	// --prune leaves alone. --prune clears a deleted branch before a branch
	// under its name arrives; --update-shallow takes new shallow roots of a
	// shallow original.
	args := []string{"fetch", "-q", "--prune", "--update-shallow", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", "--no-recurse-submodules", "--", mirror,
		"+refs/heads/*:refs/heads/*", "^refs/heads/ada", "^refs/heads/ada/*", "+refs/tags/*:refs/tags/*"}
	for _, ref := range adaRefs(strings.Fields(string(branches))) {
		args = append(args, "^"+ref)
	}
	if hasBase {
		args = append(args, "+refs/ada/base:refs/ada/base")
	}
	if src.head != "" {
		args = append(args, "+refs/ada/head:refs/ada/head")
	}
	if _, err := output(in.Git.commandWith(ctx, transport, repo, "", args...), "fetch"); err != nil {
		return "", err
	}
	if w.AfterSync != nil {
		w.AfterSync(ctx, cloned)
	}
	return configHash, nil
}

// dropAdaBranches deletes the original's branches named ada or ada/* in any
// case, which a fresh clone copies, so those names stay Ada's.
func (w *Workspaces) dropAdaBranches(ctx context.Context, repo string) error {
	g := w.Inspector.Git
	out, err := g.run(ctx, repo, "", "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return err
	}
	for _, ref := range adaRefs(strings.Fields(string(out))) {
		if _, err := g.run(ctx, repo, "", "update-ref", "--no-deref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

// adaRefs returns the refs whose first component under refs/heads/ is ada in
// any case: on a case-insensitive ref store a user's Ada or ADA/x shares a
// file or directory with Ada's own ada/<task>.
func adaRefs(refs []string) []string {
	var ada []string
	for _, ref := range refs {
		first, _, _ := strings.Cut(strings.TrimPrefix(ref, "refs/heads/"), "/")
		if strings.HasPrefix(ref, "refs/heads/") && strings.EqualFold(first, "ada") {
			ada = append(ada, ref)
		}
	}
	return ada
}

// removeLocks removes the lock files a killed clone or fetch left in the
// clone. It runs under the project's lock, where no other clone or fetch
// writes it. Locks under refs/heads/ada/ are left, as only engines write
// there; the risk left is an engine nobody confirmed had exited, still
// running git in its worktree, whose packed-refs.lock this can remove.
func removeLocks(repo string) error {
	stale, err := filepath.Glob(filepath.Join(repo, "*.lock"))
	if err != nil {
		return err
	}
	ada := filepath.Join(repo, "refs", "heads", "ada")
	err = filepath.WalkDir(filepath.Join(repo, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path == ada {
			return fs.SkipDir
		}
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(path, ".lock") {
			stale = append(stale, path)
		}
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range stale {
		if err := os.Remove(l); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeConfig replaces the clone's config with Ada's, hooks pointed at Ada's
// empty directory and fsmonitor off for any git that runs in a worktree, no
// automatic gc or maintenance (either would detach from Ada's supervision),
// and copies the mirror's excludes and attributes. It keeps a copy of the
// config outside the clone and returns its SHA-256, which the caller records
// in SQLite.
func (w *Workspaces) writeConfig(project, repo, mirror string, src mirrorSource) (string, error) {
	hooks := "hooksPath = " + quoteConfig(filepath.ToSlash(w.Inspector.Git.hooks))
	if err := writeAdaConfig(repo, src, true, "[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n", hooks, "fsmonitor = false"); err != nil {
		return "", err
	}
	cfgData, _, err := readUnder(repo, filepath.Join(repo, "config"), maxFileSize)
	if err != nil {
		return "", err
	}
	if err := writeNoFollow(w.configCopy(project), cfgData); err != nil {
		return "", err
	}
	sum := sha256.Sum256(cfgData)
	for _, name := range []string{"exclude", "attributes"} {
		dst := filepath.Join(repo, "info", name)
		data, err := os.ReadFile(filepath.Join(mirror, "info", name))
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := writeCopy(dst, data, nil); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(sum[:]), nil
}

// commitChanges applies the staged then the unstaged patch, writes the
// untracked files, and commits it all as one commit with Ada as author and
// committer; a snapshot that changes nothing makes no commit. git apply runs
// without --unsafe-paths, so it writes nothing beyond a symlink.
func (w *Workspaces) commitChanges(ctx context.Context, repo, admin, worktree string, c Changes) error {
	g := w.Inspector.Git
	for _, patch := range [][]byte{c.Staged, c.Unstaged} {
		if len(patch) == 0 {
			continue
		}
		cmd := g.inWorktree(ctx, repo, admin, worktree, "apply", "--index", "--whitespace=nowarn", "-")
		cmd.Stdin = bytes.NewReader(patch)
		if _, err := output(cmd, "apply"); err != nil {
			return err
		}
	}
	var plain, executable []string
	for _, f := range c.Untracked {
		if reason := badPath(f.Path); reason != "" || strings.HasSuffix(f.Path, "/") {
			return &PathError{Code: PathInvalid, Path: f.Path, Reason: reason}
		}
		if f.Mode != "100644" && f.Mode != "100755" {
			return &PathError{Code: PathInvalid, Path: f.Path, Reason: "mode " + f.Mode}
		}
		if err := createUnder(worktree, f.Path, f.Mode == "100755", f.Content); err != nil {
			return err
		}
		if f.Mode == "100755" {
			executable = append(executable, f.Path)
		} else {
			plain = append(plain, f.Path)
		}
	}
	for _, set := range []struct {
		chmod string
		paths []string
	}{{"--chmod=-x", plain}, {"--chmod=+x", executable}} {
		// -f: a selected file the repository ignores was taken on purpose.
		if err := g.batched(ctx, repo, admin, worktree, set.paths, "add", "-f", set.chmod, "--"); err != nil {
			return err
		}
	}
	if _, err := output(g.inWorktree(ctx, repo, admin, worktree, "diff", "--cached", "--quiet"), "diff"); err == nil {
		return nil
	} else if e, ok := errors.AsType[*exec.ExitError](err); !ok || e.ExitCode() != 1 {
		return err
	}
	cmd := g.inWorktree(ctx, repo, admin, worktree, "commit", "-q", "--no-verify", "--no-gpg-sign", "-m", importMessage)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@adanext.invalid", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@adanext.invalid")
	_, err := output(cmd, "commit")
	return err
}

// inWorktree is a git call in an attempt's worktree, run from its root: a
// path git apply writes is relative to the current directory.
func (g *Git) inWorktree(ctx context.Context, repo, admin, worktree string, args ...string) *exec.Cmd {
	return g.worktreeCommand(ctx, nil, repo, admin, worktree, args...)
}

// worktreeCommand is a git call on a worktree's git directory with the clone
// named as its common dir, so the commondir file there is never read.
func (g *Git) worktreeCommand(ctx context.Context, config []string, repo, admin, worktree string, args ...string) *exec.Cmd {
	cmd := g.commandWith(ctx, config, admin, worktree, args...)
	cmd.Env = append(cmd.Env, "GIT_COMMON_DIR="+repo)
	cmd.Dir = worktree
	return cmd
}

// sharesCommonDir reports a worktree git directory whose commondir names repo.
func sharesCommonDir(admin, repo string) bool {
	data, _, err := readUnder(admin, filepath.Join(admin, "commondir"), maxFileSize)
	if err != nil {
		return false
	}
	dir := filepath.FromSlash(strings.TrimRight(string(data), "\r\n"))
	if dir == "" {
		return false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(admin, dir)
	}
	a, err := os.Stat(dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(repo)
	return err == nil && os.SameFile(a, b)
}

// configCopy is the copy of the clone config Ada wrote, kept outside the clone.
func (w *Workspaces) configCopy(project string) string {
	return filepath.Join(w.Root, project, "ada-config")
}

// writeNoFollow replaces path with data, never writing through a link there.
func writeNoFollow(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// batched runs args in the worktree with the paths as literal pathspecs, a
// bounded number of bytes per call.
func (g *Git) batched(ctx context.Context, repo, admin, worktree string, paths []string, args ...string) error {
	for len(paths) > 0 {
		call := append([]string{}, args...)
		size := 0
		for len(paths) > 0 && (size == 0 || size+len(paths[0]) < 16<<10) {
			call = append(call, ":(literal)"+paths[0])
			size += len(paths[0]) + 11
			paths = paths[1:]
		}
		if _, err := output(g.inWorktree(ctx, repo, admin, worktree, call...), args[0]); err != nil {
			return err
		}
	}
	return nil
}

// pointsTo reports a worktree whose .git file names admin as its git directory.
func pointsTo(worktree, admin string) bool {
	data, _, err := readUnder(worktree, filepath.Join(worktree, ".git"), maxFileSize)
	if err != nil {
		return false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return false
	}
	a, err := os.Stat(filepath.FromSlash(gitDir))
	if err != nil {
		return false
	}
	b, err := os.Stat(admin)
	return err == nil && os.SameFile(a, b)
}

// Check compares an attempt's worktree, and the manifest copy in its git
// directory, with m as the workspace row holds it with hash, and the
// worktree's branch and HEAD with the prepared ones. The row records no head:
// when m has none, the copy's is used, bound by the hash. It only reads: a
// mismatch is a *MismatchError, and nothing is rebuilt.
func (w *Workspaces) Check(m Manifest, hash string) error {
	switch {
	case m.Version != ManifestVersion:
		return mismatch("manifest version %d", m.Version)
	case !safeName(m.Project) || !safeName(m.Attempt) || m.Path != w.WorktreePath(m.Project, m.Attempt):
		return mismatch("the worktree path is not the attempt's")
	}
	info, err := os.Lstat(m.Path)
	if err != nil || !info.IsDir() || isLink(m.Path, info) {
		return mismatch("the worktree is missing")
	}
	admin := w.adminDir(m.Project, m.Attempt)
	if !pointsTo(m.Path, admin) {
		return mismatch("the worktree does not use its git directory")
	}
	stored, _, err := readUnder(admin, filepath.Join(admin, manifestFile), maxFileSize)
	if err != nil {
		return mismatch("the manifest copy is missing")
	}
	if m.Head == "" {
		var copied Manifest
		if err := json.Unmarshal(stored, &copied); err != nil {
			return mismatch("the manifest copy cannot be read")
		}
		m.Head = copied.Head
	}
	data := m.Encode()
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return mismatch("the workspace row does not match its manifest hash")
	}
	if !bytes.Equal(stored, data) {
		return mismatch("the manifest copy differs from the workspace row")
	}
	head, branch, err := resolveHead(layout{gitDir: admin, commonDir: w.repo(m.Project)}, len(m.BaseCommit)/2)
	if err != nil || branch != m.Branch || head != m.Head {
		return mismatch("the worktree is not on %s at its prepared commit", m.Branch)
	}
	return nil
}
