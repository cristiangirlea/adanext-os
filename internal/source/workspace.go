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

// ManifestVersion is the version of the workspace manifest this code writes.
const ManifestVersion = 1

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
	Version    int    `json:"version"`
	Project    string `json:"project"`
	Task       string `json:"task"`
	Attempt    string `json:"attempt"`
	Snapshot   string `json:"snapshot"`
	BaseCommit string `json:"base_commit"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	Executor   string `json:"executor"`
	Policy     string `json:"policy"`
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
	afterSync func(cloned bool) // Test seam: each sync, telling a clone from a fetch.
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
// the project, task, attempt, snapshot, base commit, executor and policy; the
// returned manifest adds the version, branch and path.
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
	if err := w.sync(ctx, m.Project, source); err != nil {
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
	if !pointsTo(m.Path, admin) {
		return Manifest{}, fmt.Errorf("the worktree at %s does not use %s", m.Path, admin)
	}
	if err := w.commitChanges(ctx, admin, m.Path, changes); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(admin, manifestFile), m.Encode(), 0o600); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// sync clones the project's repository from a freshly built mirror the first
// time, then fetches from one, and gives the clone Ada's config. Neither call
// reads the original's config, and --no-local makes pack-objects copy what
// the mirror reaches through alternates into the clone's own pack.
func (w *Workspaces) sync(ctx context.Context, project, source string) error {
	in := w.Inspector
	dir := filepath.Join(w.Root, project)
	for _, d := range []string{dir, in.MirrorRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	mirror, err := os.MkdirTemp(in.MirrorRoot, "w")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mirror)
	r, src, err := in.inspectMirror(ctx, source, mirror)
	if err != nil {
		return err
	}
	if src == nil {
		return &BlockedError{Blockers: r.Blockers}
	}
	transport := []string{"protocol.file.allow=always", "transfer.bundleURI=false"}
	repo := w.repo(project)
	_, err = os.Lstat(repo)
	cloned := errors.Is(err, fs.ErrNotExist)
	if err != nil && !cloned {
		return err
	}
	if cloned {
		// A clone a crash interrupted never reached repo.git.
		stale, _ := filepath.Glob(filepath.Join(dir, "clone-*"))
		for _, s := range stale {
			if err := os.RemoveAll(s); err != nil {
				return err
			}
		}
		tmp, err := os.MkdirTemp(dir, "clone-")
		if err != nil {
			return err
		}
		if _, err := output(in.Git.commandWith(ctx, transport, "", "", "clone", "-q", "--bare", "--no-local", "--no-hardlinks", "--template=", "--", mirror, tmp), "clone"); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		if err := w.writeConfig(tmp, mirror, *src); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		if err := os.Rename(tmp, repo); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	} else if err := w.writeConfig(repo, mirror, *src); err != nil {
		return err
	}
	// refs/ada/head carries a detached HEAD no branch reaches; ada/* are Ada's own branches.
	args := []string{"fetch", "-q", "--no-tags", "--no-write-fetch-head", "--no-auto-gc", "--no-recurse-submodules", "--", mirror,
		"+refs/heads/*:refs/heads/*", "^refs/heads/ada/*", "+refs/tags/*:refs/tags/*"}
	if src.head != "" {
		args = append(args, "+refs/ada/head:refs/ada/head")
	}
	if _, err := output(in.Git.commandWith(ctx, transport, repo, "", args...), "fetch"); err != nil {
		return err
	}
	if w.afterSync != nil {
		w.afterSync(cloned)
	}
	return nil
}

// writeConfig replaces the clone's config with Ada's, hooks pointed at Ada's
// empty directory and fsmonitor off for any git that runs in a worktree, and
// copies the mirror's excludes and attributes.
func (w *Workspaces) writeConfig(repo, mirror string, src mirrorSource) error {
	hooks := "hooksPath = " + quoteConfig(filepath.ToSlash(w.Inspector.Git.hooks))
	if err := writeAdaConfig(repo, src, true, hooks, "fsmonitor = false"); err != nil {
		return err
	}
	for _, name := range []string{"exclude", "attributes"} {
		dst := filepath.Join(repo, "info", name)
		data, err := os.ReadFile(filepath.Join(mirror, "info", name))
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := writeCopy(dst, data, nil); err != nil {
			return err
		}
	}
	return nil
}

// commitChanges applies the staged then the unstaged patch, writes the
// untracked files, and commits it all as one commit with Ada as author and
// committer; a snapshot that changes nothing makes no commit. git apply runs
// without --unsafe-paths, so it writes nothing beyond a symlink.
func (w *Workspaces) commitChanges(ctx context.Context, admin, worktree string, c Changes) error {
	g := w.Inspector.Git
	for _, patch := range [][]byte{c.Staged, c.Unstaged} {
		if len(patch) == 0 {
			continue
		}
		cmd := g.inWorktree(ctx, admin, worktree, "apply", "--index", "--whitespace=nowarn", "-")
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
		if err := g.batched(ctx, admin, worktree, set.paths, "add", "-f", set.chmod, "--"); err != nil {
			return err
		}
	}
	if _, err := output(g.inWorktree(ctx, admin, worktree, "diff", "--cached", "--quiet"), "diff"); err == nil {
		return nil
	} else if e, ok := errors.AsType[*exec.ExitError](err); !ok || e.ExitCode() != 1 {
		return err
	}
	cmd := g.inWorktree(ctx, admin, worktree, "commit", "-q", "--no-verify", "--no-gpg-sign", "-m", importMessage)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@adanext.invalid", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@adanext.invalid")
	_, err := output(cmd, "commit")
	return err
}

// inWorktree is a git call in an attempt's worktree, run from its root: a
// path git apply writes is relative to the current directory.
func (g *Git) inWorktree(ctx context.Context, admin, worktree string, args ...string) *exec.Cmd {
	cmd := g.command(ctx, admin, worktree, args...)
	cmd.Dir = worktree
	return cmd
}

// batched runs args in the worktree with the paths as literal pathspecs, a
// bounded number of bytes per call.
func (g *Git) batched(ctx context.Context, admin, worktree string, paths []string, args ...string) error {
	for len(paths) > 0 {
		call := append([]string{}, args...)
		size := 0
		for len(paths) > 0 && (size == 0 || size+len(paths[0]) < 16<<10) {
			call = append(call, ":(literal)"+paths[0])
			size += len(paths[0]) + 11
			paths = paths[1:]
		}
		if _, err := output(g.inWorktree(ctx, admin, worktree, call...), args[0]); err != nil {
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
// directory, with m as the workspace row holds it with hash. It only reads:
// a mismatch is a *MismatchError, and nothing is rebuilt.
func (w *Workspaces) Check(m Manifest, hash string) error {
	data := m.Encode()
	sum := sha256.Sum256(data)
	switch {
	case hex.EncodeToString(sum[:]) != hash:
		return mismatch("the workspace row does not match its manifest hash")
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
	if !bytes.Equal(stored, data) {
		return mismatch("the manifest copy differs from the workspace row")
	}
	return nil
}
