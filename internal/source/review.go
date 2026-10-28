package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RenameThreshold is the fixed similarity git needs to report a rename in Changes.
const RenameThreshold = "50%"

// ChangeLimits caps what Changes returns: files listed, patch bytes per file and in all.
type ChangeLimits struct {
	Files      int
	FileBytes  int64
	TotalBytes int64
}

// DefaultChangeLimits are the caps the changes route uses.
var DefaultChangeLimits = ChangeLimits{Files: 1000, FileBytes: 512 << 10, TotalBytes: 8 << 20}

// ChangedFile is one file of a diff, its paths the bytes git reported.
type ChangedFile struct {
	Path, OldPath    string // OldPath is set for a rename.
	Status           string // A, D, M, R or T, as git reports it.
	Similarity       int    // A rename's similarity, in percent.
	OldMode, NewMode string
	Binary           bool
	Added, Deleted   int // -1 for a binary file.
	Patch            []byte
	PatchTruncated   bool
}

// ChangeSet is the diff between two commits of Ada's clone.
type ChangeSet struct {
	From, To       string
	Files          []ChangedFile
	Total          int  // Files in the diff, listed or not.
	FilesTruncated bool // More files than the cap.
	BytesTruncated bool // A patch was cut by the cap on all bytes.
}

func validObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && isHex(id)
}

// diffArgs are the flags every Changes call gives git diff: no external diff or textconv (the
// runner adds both), NUL-separated output, a fixed rename threshold and no copies.
func diffArgs(format string, from, to string) []string {
	return []string{"diff", format, "-z", "-M" + RenameThreshold, "--no-color", "--no-relative", "--full-index", "--src-prefix=a/", "--dst-prefix=b/", from, to, "--"}
}

// Changes is the diff from one commit of the project's clone to another, through Ada's git
// runner with the clone config checked first; patches and the file list are capped by lim, and
// each cut is marked.
func (w *Workspaces) Changes(ctx context.Context, project, from, to, configSHA256 string, lim ChangeLimits) (ChangeSet, error) {
	if !safeName(project) || !validObjectID(from) || !validObjectID(to) {
		return ChangeSet{}, fmt.Errorf("invalid changes request %q %q %q", project, from, to)
	}
	defer w.lock(project)()
	if err := w.verifyCloneConfig(project, configSHA256, ""); err != nil {
		return ChangeSet{}, err
	}
	g, repo, flags := w.Inspector.Git, w.repo(project), w.safeConfigFlags()
	raw, err := output(g.commandWith(ctx, flags, repo, "", diffArgs("--raw", from, to)...), "diff")
	if err != nil {
		return ChangeSet{}, err
	}
	files, err := parseRaw(raw)
	if err != nil {
		return ChangeSet{}, err
	}
	numstat, err := output(g.commandWith(ctx, flags, repo, "", diffArgs("--numstat", from, to)...), "diff")
	if err != nil {
		return ChangeSet{}, err
	}
	if err := applyNumstat(files, numstat); err != nil {
		return ChangeSet{}, err
	}
	set := ChangeSet{From: from, To: to, Total: len(files)}
	if lim.Files > 0 && len(files) > lim.Files {
		files, set.FilesTruncated = files[:lim.Files], true
	}
	set.Files = files
	if len(files) == 0 {
		set.Files = []ChangedFile{}
		return set, nil
	}
	set.BytesTruncated, err = w.readPatches(ctx, repo, from, to, set.Files, lim)
	return set, err
}

// parseRaw reads git diff --raw -z: ":<old> <new> <oid> <oid> <status>" then the path, or for a
// rename the old and the new path.
func parseRaw(out []byte) ([]ChangedFile, error) {
	tokens := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	var files []ChangedFile
	for i := 0; i < len(tokens); i++ {
		if len(tokens[i]) == 0 && len(out) == 0 {
			break
		}
		meta := strings.Fields(strings.TrimPrefix(string(tokens[i]), ":"))
		if len(meta) != 5 || i+1 >= len(tokens) {
			return nil, fmt.Errorf("unexpected git diff --raw record %q", tokens[i])
		}
		f := ChangedFile{OldMode: meta[0], NewMode: meta[1], Status: meta[4][:1]}
		if f.Status == "R" || f.Status == "C" {
			if i+2 >= len(tokens) {
				return nil, fmt.Errorf("a rename record without its paths")
			}
			f.Similarity, _ = strconv.Atoi(meta[4][1:])
			f.OldPath, f.Path = string(tokens[i+1]), string(tokens[i+2])
			i += 2
		} else {
			f.Path = string(tokens[i+1])
			i++
		}
		files = append(files, f)
	}
	return files, nil
}

// applyNumstat reads git diff --numstat -z, in the same order as --raw: "<added>\t<deleted>\t"
// then the path, or an empty path and the old and new paths for a rename; "-" marks binary.
func applyNumstat(files []ChangedFile, out []byte) error {
	tokens := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	n := 0
	for i := 0; i < len(tokens) && len(out) > 0; i++ {
		parts := strings.SplitN(string(tokens[i]), "\t", 3)
		if len(parts) != 3 || n >= len(files) {
			return fmt.Errorf("unexpected git diff --numstat record %q", tokens[i])
		}
		path := parts[2]
		if path == "" {
			if i+2 >= len(tokens) {
				return fmt.Errorf("a numstat rename without its paths")
			}
			path = string(tokens[i+2])
			i += 2
		}
		if path != files[n].Path {
			return fmt.Errorf("git diff --numstat and --raw disagree at %q", path)
		}
		if parts[0] == "-" && parts[1] == "-" {
			files[n].Binary, files[n].Added, files[n].Deleted = true, -1, -1
		} else {
			files[n].Added, _ = strconv.Atoi(parts[0])
			files[n].Deleted, _ = strconv.Atoi(parts[1])
		}
		n++
	}
	if n != len(files) {
		return fmt.Errorf("git diff --numstat listed %d files, --raw %d", n, len(files))
	}
	return nil
}

// readPatches reads git's patch and gives each listed file its part, the text from its
// "diff --git" line to the next one; a hunk line always starts with a prefix character, so only
// a header starts that way. Reading stops at the cap on all bytes, and the rest is marked cut.
func (w *Workspaces) readPatches(ctx context.Context, repo, from, to string, files []ChangedFile, lim ChangeLimits) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := w.Inspector.Git.commandWith(ctx, w.safeConfigFlags(), repo, "", diffArgs("-p", from, to)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	header := []byte("diff --git ")
	index, total, lineStart, cut := -1, int64(0), true, false
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			if lineStart && bytes.HasPrefix(part, header) {
				index++
			}
			lineStart = part[len(part)-1] == '\n'
			if index >= len(files) {
				break // past the listed files
			}
			if index >= 0 {
				f := &files[index]
				take := int64(len(part))
				if lim.FileBytes > 0 && int64(len(f.Patch))+take > lim.FileBytes {
					take = max(0, lim.FileBytes-int64(len(f.Patch)))
					f.PatchTruncated = true
				}
				if lim.TotalBytes > 0 && total+take > lim.TotalBytes {
					take = max(0, lim.TotalBytes-total)
					f.PatchTruncated, cut = true, true
				}
				f.Patch = append(f.Patch, part[:take]...)
				total += take
				if cut {
					break
				}
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return false, err
			}
			break
		}
	}
	stopped := cut || index >= len(files)
	if stopped {
		cancel()
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if !stopped && waitErr != nil {
		return false, fmt.Errorf("git diff: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if cut {
		for i := index + 1; i < len(files); i++ {
			files[i].PatchTruncated = true
		}
	} else if !stopped && index != len(files)-1 {
		return false, fmt.Errorf("git diff gave %d patches for %d files", index+1, len(files))
	}
	return cut, nil
}

// ErrNoSuchPath is a path the commit does not hold, or one through a file or a link.
var ErrNoSuchPath = errors.New("no such path in the commit")

// TreeEntry is one entry of a directory in a commit; Type is blob, tree, symlink or submodule.
type TreeEntry struct {
	Name, Path, Type, Mode string
	Size                   int64 // -1 for a tree or a submodule.
}

// FileView is what Files reads at a path: a directory's entries, a file's content, or a link's
// target, which is never followed.
type FileView struct {
	Commit, Path, Type, Mode string
	Entries                  []TreeEntry
	EntriesTruncated         bool
	Size                     int64
	Content                  []byte
	Truncated                bool
	Target                   string // A symlink's target, as stored.
}

// validTreePath is a slash path inside a commit, "" for its root.
func validTreePath(p string) bool {
	if p == "" {
		return true
	}
	if strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func entryType(mode string) string {
	switch mode {
	case "040000":
		return "tree"
	case "120000":
		return "symlink"
	case "160000":
		return "submodule"
	}
	return "blob"
}

// lsTree lists one tree object, NUL-separated: "<mode> <type> <oid> <size>\t<name>".
func (w *Workspaces) lsTree(ctx context.Context, repo, tree string) ([]TreeEntry, []string, error) {
	out, err := output(w.Inspector.Git.commandWith(ctx, w.safeConfigFlags(), repo, "", "ls-tree", "-z", "-l", "--full-tree", tree), "ls-tree")
	if err != nil {
		return nil, nil, err
	}
	var entries []TreeEntry
	var oids []string
	for _, rec := range bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 4 {
			return nil, nil, fmt.Errorf("unexpected git ls-tree record %q", rec)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			size = -1
		}
		entries = append(entries, TreeEntry{Name: string(name), Type: entryType(fields[0]), Mode: fields[0], Size: size})
		oids = append(oids, fields[2])
	}
	return entries, oids, nil
}

// readBlob reads at most limit bytes of a blob, reporting whether it holds more.
func (w *Workspaces) readBlob(ctx context.Context, repo, oid string, limit int64) ([]byte, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := w.Inspector.Git.commandWith(ctx, w.safeConfigFlags(), repo, "", "cat-file", "blob", oid)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(stdout, limit+1))
	more := int64(len(data)) > limit
	if more {
		data = data[:limit]
		cancel()
	}
	_, _ = io.Copy(io.Discard, stdout)
	if waitErr := cmd.Wait(); err == nil && !more {
		err = waitErr
	}
	return data, more, err
}

// Files reads path in commit of the project's clone through git alone (ls-tree, cat-file), never
// through a worktree, so no link is followed: a symlink is shown as one, with its stored target.
// A directory lists at most maxEntries entries, a file at most maxBytes of its content.
func (w *Workspaces) Files(ctx context.Context, project, commit, path, configSHA256 string, maxEntries int, maxBytes int64) (FileView, error) {
	if !safeName(project) || !validObjectID(commit) || !validTreePath(path) {
		return FileView{}, ErrNoSuchPath
	}
	defer w.lock(project)()
	if err := w.verifyCloneConfig(project, configSHA256, ""); err != nil {
		return FileView{}, err
	}
	g, repo := w.Inspector.Git, w.repo(project)
	treeOut, err := output(g.commandWith(ctx, w.safeConfigFlags(), repo, "", "rev-parse", "--verify", "--quiet", commit+"^{tree}"), "rev-parse")
	if err != nil {
		return FileView{}, err
	}
	view := FileView{Commit: commit, Path: path, Type: "tree", Mode: "040000"}
	oid := strings.TrimSpace(string(treeOut))
	var parts []string
	if path != "" {
		parts = strings.Split(path, "/")
	}
	for i, part := range parts {
		entries, oids, err := w.lsTree(ctx, repo, oid)
		if err != nil {
			return FileView{}, err
		}
		found := -1
		for j, e := range entries {
			if e.Name == part {
				found = j
				break
			}
		}
		if found < 0 || (i < len(parts)-1 && entries[found].Type != "tree") {
			return FileView{}, ErrNoSuchPath
		}
		oid, view.Type, view.Mode, view.Size = oids[found], entries[found].Type, entries[found].Mode, entries[found].Size
	}
	switch view.Type {
	case "tree":
		view.Size = -1
		entries, _, err := w.lsTree(ctx, repo, oid)
		if err != nil {
			return FileView{}, err
		}
		if maxEntries > 0 && len(entries) > maxEntries {
			entries, view.EntriesTruncated = entries[:maxEntries], true
		}
		for i := range entries {
			entries[i].Path = entries[i].Name
			if path != "" {
				entries[i].Path = path + "/" + entries[i].Name
			}
		}
		view.Entries = entries
		if view.Entries == nil {
			view.Entries = []TreeEntry{}
		}
	case "symlink":
		target, more, err := w.readBlob(ctx, repo, oid, 4096)
		if err != nil {
			return FileView{}, err
		}
		view.Target, view.Truncated = string(target), more
	case "blob":
		view.Content, view.Truncated, err = w.readBlob(ctx, repo, oid, maxBytes)
		if err != nil {
			return FileView{}, err
		}
	}
	return view, nil
}

// realDir reports a path that is a directory itself, not a link or a reparse point to one.
func realDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && !isLink(path, info)
}

// RemoveWorktree removes an ended attempt's worktree: only <Root>/<project>/worktrees/<attempt>,
// built from ids, never following a link or reparse point in it (the directory itself replaced
// by one is removed as the link alone), then only that attempt's admin directory under the
// clone's worktrees/, never through git worktree prune. A step finds what it removes gone after
// a crash and goes on.
func (w *Workspaces) RemoveWorktree(project, attempt string) error {
	if !safeName(project) || !safeName(attempt) {
		return fmt.Errorf("identifier %q or %q cannot name a worktree", project, attempt)
	}
	defer w.lock(project)()
	dir := filepath.Join(w.Root, project)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !realDir(dir) {
		return mismatch("the project's state directory is not a real directory")
	}
	parent := filepath.Dir(w.WorktreePath(project, attempt))
	if _, err := os.Lstat(parent); err == nil {
		if !realDir(parent) {
			return mismatch("the worktrees directory is not a real directory")
		}
		if err := removeTreeNoFollow(w.WorktreePath(project, attempt)); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if w.afterWorktreeRemoved != nil {
		if err := w.afterWorktreeRemoved(); err != nil {
			return err
		}
	}
	repo := w.repo(project)
	admins := filepath.Join(repo, "worktrees")
	if _, err := os.Lstat(admins); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !realDir(repo) || !realDir(admins) {
		return mismatch("the clone's worktrees directory is not a real directory")
	}
	return removeTreeNoFollow(w.adminDir(project, attempt))
}

// Continue gives a refined attempt its worktree on the task's existing branch ada/<task>, from
// its tip: the branch must be at tip, the agent-changes commit of the attempt before, and descend
// from the base commit. Nothing of the snapshot is applied again. The clone is not fetched: its
// config is checked against m.ConfigSHA256, Ada's recorded copy.
func (w *Workspaces) Continue(ctx context.Context, m Manifest, tip string) (Manifest, error) {
	for _, id := range []string{m.Project, m.Task, m.Attempt} {
		if !safeName(id) {
			return Manifest{}, fmt.Errorf("identifier %q cannot name a workspace", id)
		}
	}
	if !validObjectID(m.BaseCommit) || !validObjectID(tip) {
		return Manifest{}, fmt.Errorf("base %q or tip %q is not an object ID", m.BaseCommit, tip)
	}
	defer w.lock(m.Project)()
	if err := w.verifyCloneConfig(m.Project, m.ConfigSHA256, ""); err != nil {
		return Manifest{}, err
	}
	g, repo, flags := w.Inspector.Git, w.repo(m.Project), w.safeConfigFlags()
	m.Version, m.Branch, m.Path = ManifestVersion, "ada/"+m.Task, w.WorktreePath(m.Project, m.Attempt)
	if current := w.BranchTip(m.Project, m.Task); current != tip {
		return Manifest{}, mismatch("the branch %s is at %q, not at the earlier attempt's commit", m.Branch, current)
	}
	if _, err := output(g.commandWith(ctx, flags, repo, "", "merge-base", "--is-ancestor", m.BaseCommit, tip), "merge-base"); err != nil {
		return Manifest{}, mismatch("the branch %s no longer descends from the base commit", m.Branch)
	}
	admin := w.adminDir(m.Project, m.Attempt)
	for _, p := range []string{m.Path, admin} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%s already exists", p)
		}
	}
	if !realDir(filepath.Dir(m.Path)) {
		if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil || !realDir(filepath.Dir(m.Path)) {
			return Manifest{}, mismatch("the worktrees directory is not a real directory")
		}
	}
	if _, err := output(g.commandWith(ctx, flags, repo, "", "worktree", "add", "-q", "--lock", "--reason", "Ada attempt "+m.Attempt, m.Path, m.Branch), "worktree"); err != nil {
		return Manifest{}, err
	}
	if !pointsTo(m.Path, admin) || !sharesCommonDir(admin, repo) {
		return Manifest{}, fmt.Errorf("the worktree at %s does not use %s", m.Path, admin)
	}
	head, branch, err := resolveHead(layout{gitDir: admin, commonDir: repo}, len(m.BaseCommit)/2)
	if err != nil || branch != m.Branch || head != tip {
		return Manifest{}, fmt.Errorf("the worktree's HEAD is not %s on %s: %q %q, %v", tip, m.Branch, head, branch, err)
	}
	m.Head = head
	if err := os.WriteFile(filepath.Join(admin, manifestFile), m.Encode(), 0o600); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
