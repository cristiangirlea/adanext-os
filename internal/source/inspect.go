package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Blocker codes: why a repository cannot be imported as it stands.
const (
	BlockNotRepository    = "not_git_repository"
	BlockBare             = "bare_repository"
	BlockWorktree         = "core_worktree"
	BlockPartialClone     = "partial_clone"
	BlockReftable         = "reftable"
	BlockUnknownExtension = "unknown_extension"
	BlockSplitIndex       = "split_index"
	BlockSparse           = "sparse_checkout"
	BlockMerge            = "merge_in_progress"
	BlockRebase           = "rebase_in_progress"
	BlockCherryPick       = "cherry_pick_in_progress"
	BlockRevert           = "revert_in_progress"
	BlockBisect           = "bisect_in_progress"
	BlockUnmerged         = "unmerged_entries"
	BlockSubmodule        = "submodule"
	BlockLFS              = "git_lfs"
	BlockEscapingLink     = "escaping_link"
	BlockRemotePath       = "unc_path"
	BlockAssumeUnchanged  = "assume_unchanged"
)

// maxBlockerPaths bounds a blocker's listed paths; Count keeps the total.
const maxBlockerPaths = 100

// Blocker is a reason the repository cannot be imported, with the work-tree
// paths, files or config keys involved.
type Blocker struct {
	Code  string   `json:"code"`
	Paths []string `json:"paths"`
	Count int      `json:"count"`
}

type Remote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Identity struct {
	Path      string   `json:"path"`
	GitDir    string   `json:"git_dir"`
	CommonDir string   `json:"common_dir"`
	Head      string   `json:"head"`   // empty on an unborn branch
	Branch    string   `json:"branch"` // empty when detached
	Detached  bool     `json:"detached"`
	Remotes   []Remote `json:"remotes"`
}

// Change is a path and its one-letter porcelain status (M, A, D, T).
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type Status struct {
	Staged    []Change `json:"staged"`
	Unstaged  []Change `json:"unstaged"`
	Untracked []string `json:"untracked"`
	Unmerged  []string `json:"unmerged"`
}

type Commit struct {
	Commit  string   `json:"commit"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Email   string   `json:"email"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
}

type Inspection struct {
	Identity    Identity  `json:"identity"`
	Status      Status    `json:"status"`
	History     []Commit  `json:"history"`
	Files       []string  `json:"files"`
	Fingerprint string    `json:"fingerprint"`
	Blockers    []Blocker `json:"blockers"`
}

// Inspector inspects repositories with a resolved git, building each mirror
// in a fresh directory under MirrorRoot and removing it afterwards.
type Inspector struct {
	Git        *Git
	MirrorRoot string
	Config     ConfigFiles
	// Test seams in Capture: after its first inspection, and before the second.
	afterInspect, beforeRecheck func()
}

// Inspect reads the repository whose work tree root is path. Blockers are
// part of the result; an error means the repository could not be inspected.
func (in *Inspector) Inspect(ctx context.Context, path string) (Inspection, error) {
	if err := os.MkdirAll(in.MirrorRoot, 0o700); err != nil {
		return Inspection{}, err
	}
	mirror, err := os.MkdirTemp(in.MirrorRoot, "m")
	if err != nil {
		return Inspection{}, err
	}
	defer os.RemoveAll(mirror)
	return in.inspect(ctx, path, mirror)
}

func newInspection() Inspection {
	return Inspection{
		Identity: Identity{Remotes: []Remote{}},
		Status:   Status{Staged: []Change{}, Unstaged: []Change{}, Untracked: []string{}, Unmerged: []string{}},
		History:  []Commit{}, Files: []string{}, Blockers: []Blocker{},
	}
}

func (r *Inspection) block(code string, paths ...string) {
	b := Blocker{Code: code, Paths: paths, Count: len(paths)}
	if len(paths) > maxBlockerPaths {
		b.Paths = paths[:maxBlockerPaths]
	}
	if b.Paths == nil {
		b.Paths = []string{}
	}
	r.Blockers = append(r.Blockers, b)
}

// stopping blockers leave nothing git could read correctly through a mirror.
var stopping = map[string]bool{BlockNotRepository: true, BlockBare: true, BlockRemotePath: true, BlockReftable: true, BlockUnknownExtension: true, BlockPartialClone: true, BlockSplitIndex: true}

func (r *Inspection) stopped() bool {
	for _, b := range r.Blockers {
		if stopping[b.Code] {
			return true
		}
	}
	return false
}

func (in *Inspector) inspect(ctx context.Context, path, mirror string) (Inspection, error) {
	r := newInspection()
	if !filepath.IsAbs(path) {
		return r, fmt.Errorf("path %q is not absolute", path)
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.Identity.Path = filepath.Clean(path)
			r.block(BlockNotRepository, r.Identity.Path)
			return r, nil
		}
		return r, err
	}
	r.Identity.Path = root
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		r.block(BlockNotRepository, root)
		return r, nil
	}
	l, b := readLayout(root)
	r.Identity.GitDir, r.Identity.CommonDir = l.gitDir, l.commonDir
	if b != nil {
		r.block(b.Code, b.Paths...)
		return r, nil
	}

	repo, err := readConfigFile(l.commonDir, filepath.Join(l.commonDir, "config"))
	if err != nil {
		return r, err
	}
	if repo.boolean("extensions.worktreeConfig") {
		wt, err := readConfigFile(l.gitDir, filepath.Join(l.gitDir, "config.worktree"))
		if err != nil {
			return r, err
		}
		repo.merge(wt)
	}
	merged := &config{}
	for _, file := range append(append([]string{}, in.Config.System...), in.Config.Global...) {
		c, err := readConfigFile("", file)
		if err != nil {
			return r, err
		}
		merged.merge(c)
	}
	merged.merge(repo)
	hashSize := r.checkConfig(repo)
	r.Identity.Remotes = append(r.Identity.Remotes, remotes(merged)...)
	if r.stopped() {
		return r, nil
	}

	indexData, indexTime, idx, err := readIndex(l.gitDir, hashSize)
	if err != nil {
		return r, err
	}
	r.checkIndex(idx, repo)
	r.checkOperations(l.gitDir)
	r.Identity.Head, r.Identity.Branch, err = resolveHead(l, hashSize)
	if err != nil {
		return r, err
	}
	r.Identity.Detached = r.Identity.Branch == "" && r.Identity.Head != ""
	if r.stopped() {
		return r, nil
	}

	src := mirrorSource{layout: l, repo: repo, merged: merged, files: in.Config, index: indexData, indexModTime: indexTime}
	if err := writeMirror(mirror, src); err != nil {
		return r, fmt.Errorf("build mirror: %w", err)
	}
	out, err := in.Git.run(ctx, mirror, root, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return r, err
	}
	r.Status = parseStatus(out)
	if r.Identity.Head != "" {
		out, err := in.Git.run(ctx, mirror, root, "log", "-n", "20", "--no-decorate", "--format=%H%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e", "HEAD", "--")
		if err != nil {
			return r, err
		}
		r.History = parseLog(out)
	}
	links := r.checkLinks(root, idx)
	if err := r.checkLFS(root, l.commonDir, idx, links); err != nil {
		return r, err
	}
	r.Files = instructionFiles(root)
	r.Fingerprint = fingerprint(root, r.Identity.Head, idx, r.Status, links)
	return r, nil
}

// checkConfig records the repository config's blockers and returns the
// object ID size.
func (r *Inspection) checkConfig(repo *config) int {
	hashSize := 20
	if v, _ := repo.get("core.repositoryformatversion"); v != "" && v != "0" && v != "1" {
		r.block(BlockUnknownExtension, "core.repositoryformatversion")
	}
	if repo.boolean("core.bare") {
		r.block(BlockBare, "core.bare")
	}
	if _, ok := repo.get("core.worktree"); ok {
		r.block(BlockWorktree, "core.worktree")
	}
	var unknown []string
	for _, e := range repo.entries {
		if e.section != "extensions" || e.subsection != "" {
			continue
		}
		value := strings.ToLower(e.value)
		switch e.name {
		case "objectformat":
			switch value {
			case "sha1":
			case "sha256":
				hashSize = 32
			default:
				unknown = append(unknown, "extensions.objectformat")
			}
		case "refstorage":
			switch value {
			case "files":
			case "reftable":
				r.block(BlockReftable, "extensions.refstorage")
			default:
				unknown = append(unknown, "extensions.refstorage")
			}
		case "partialclone":
			r.block(BlockPartialClone, "extensions.partialclone")
		case "worktreeconfig", "preciousobjects", "noop", "noop-v1":
		default:
			unknown = append(unknown, "extensions."+e.name)
		}
	}
	if len(unknown) > 0 {
		r.block(BlockUnknownExtension, unknown...)
	}
	return hashSize
}

func remotes(c *config) []Remote {
	var out []Remote
	for _, e := range c.entries {
		if e.section == "remote" && e.subsection != "" && e.name == "url" && e.hasValue {
			out = append(out, Remote{Name: e.subsection, URL: e.value})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readIndex copies gitDir's index, holding its handle only for the copy, and
// retries a copy that raced the user's git replacing it.
func readIndex(gitDir string, hashSize int) ([]byte, time.Time, *index, error) {
	path := filepath.Join(gitDir, "index")
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
		}
		data, info, err := readUnder(gitDir, path, maxIndexSize)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, time.Time{}, &index{}, nil
		}
		if err == nil {
			var x *index
			if x, err = parseIndex(data, hashSize); err == nil {
				return data, info.ModTime(), x, nil
			}
		}
		lastErr = err
	}
	return nil, time.Time{}, nil, fmt.Errorf("read index: %w", lastErr)
}

func (r *Inspection) checkIndex(idx *index, repo *config) {
	if idx.has("link") {
		r.block(BlockSplitIndex, "index")
	}
	var unmerged, gitlinks, skipped, assumed []string
	for _, e := range idx.entries {
		if e.stage != 0 {
			if len(unmerged) == 0 || unmerged[len(unmerged)-1] != e.path {
				unmerged = append(unmerged, e.path)
			}
			continue
		}
		if e.mode&modeTypeMask == modeGitlink {
			gitlinks = append(gitlinks, e.path)
		}
		if e.skipWork {
			skipped = append(skipped, e.path)
		}
		if e.assumeValid {
			// git never compares these, so their edits are missing from status and snapshots.
			assumed = append(assumed, e.path)
		}
	}
	var sparse []string
	if repo.boolean("core.sparseCheckout") {
		sparse = append(sparse, "core.sparseCheckout")
	}
	if idx.has("sdir") {
		sparse = append(sparse, "index")
	}
	if sparse = append(sparse, skipped...); len(sparse) > 0 {
		r.block(BlockSparse, sparse...)
	}
	if len(unmerged) > 0 {
		r.block(BlockUnmerged, unmerged...)
	}
	if len(gitlinks) > 0 {
		r.block(BlockSubmodule, gitlinks...)
	}
	if len(assumed) > 0 {
		r.block(BlockAssumeUnchanged, assumed...)
	}
}

func (r *Inspection) checkOperations(gitDir string) {
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(gitDir, name))
		return err == nil
	}
	if exists("MERGE_HEAD") {
		r.block(BlockMerge, "MERGE_HEAD")
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if exists(name) {
			r.block(BlockRebase, name)
		}
	}
	if exists("CHERRY_PICK_HEAD") {
		r.block(BlockCherryPick, "CHERRY_PICK_HEAD")
	}
	if exists("REVERT_HEAD") {
		r.block(BlockRevert, "REVERT_HEAD")
	}
	if exists("sequencer") && !exists("CHERRY_PICK_HEAD") && !exists("REVERT_HEAD") {
		// A paused multi-commit pick or revert; its todo says which.
		todo, _, _ := readUnder(gitDir, filepath.Join(gitDir, "sequencer", "todo"), maxFileSize)
		if bytes.HasPrefix(bytes.TrimSpace(todo), []byte("revert")) {
			r.block(BlockRevert, "sequencer")
		} else {
			r.block(BlockCherryPick, "sequencer")
		}
	}
	if exists("BISECT_LOG") {
		r.block(BlockBisect, "BISECT_LOG")
	}
}

// resolveHead reads HEAD and the ref it names, as files, never through git.
func resolveHead(l layout, hashSize int) (commit, branch string, err error) {
	data, _, err := readUnder(l.gitDir, filepath.Join(l.gitDir, "HEAD"), maxFileSize)
	if err != nil {
		return "", "", err
	}
	value := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(value, "ref: "); ok {
		branch = strings.TrimPrefix(ref, "refs/heads/")
		for depth := 0; ; depth++ {
			if depth > 5 {
				return "", "", errors.New("HEAD: symbolic ref chain too deep")
			}
			value, err = readRef(l, ref)
			if err != nil || value == "" {
				return "", branch, err
			}
			next, symbolic := strings.CutPrefix(value, "ref: ")
			if !symbolic {
				break
			}
			ref = next
		}
	}
	if len(value) != 2*hashSize || !isHex(value) {
		return "", "", fmt.Errorf("HEAD: not an object ID: %q", value)
	}
	return value, branch, nil
}

// readRef reads a ref from its loose file or packed-refs; "" when unborn.
func readRef(l layout, ref string) (string, error) {
	if strings.Contains(ref, "..") || !strings.HasPrefix(ref, "refs/") {
		return "", fmt.Errorf("bad ref %q", ref)
	}
	dir := l.commonDir
	for _, own := range []string{"refs/bisect/", "refs/worktree/", "refs/rewritten/"} {
		if strings.HasPrefix(ref, own) {
			dir = l.gitDir
		}
	}
	data, _, err := readUnder(dir, filepath.Join(dir, filepath.FromSlash(ref)), maxFileSize)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	packed, _, err := readUnder(l.commonDir, filepath.Join(l.commonDir, "packed-refs"), maxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if oid, name, ok := strings.Cut(strings.TrimRight(line, "\r"), " "); ok && name == ref {
			return oid, nil
		}
	}
	return "", nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func parseStatus(out []byte) Status {
	s := newInspection().Status
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 2 {
			continue
		}
		var xy, path string
		switch rec[0] {
		case '1':
			if f := strings.SplitN(rec, " ", 9); len(f) == 9 {
				xy, path = f[1], f[8]
			}
		case '2':
			if f := strings.SplitN(rec, " ", 10); len(f) == 10 {
				xy, path = f[1], f[9]
			}
			i++ // the original path follows as its own record
		case 'u':
			if f := strings.SplitN(rec, " ", 11); len(f) == 11 {
				s.Unmerged = append(s.Unmerged, f[10])
			}
			continue
		case '?':
			s.Untracked = append(s.Untracked, rec[2:])
			continue
		}
		if len(xy) != 2 {
			continue
		}
		if xy[0] != '.' {
			s.Staged = append(s.Staged, Change{Path: path, Status: xy[:1]})
		}
		if xy[1] != '.' {
			s.Unstaged = append(s.Unstaged, Change{Path: path, Status: xy[1:]})
		}
	}
	return s
}

func parseLog(out []byte) []Commit {
	commits := []Commit{}
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.Split(rec, "\x1f")
		if len(f) != 6 {
			continue
		}
		c := Commit{Commit: f[0], Parents: strings.Fields(f[1]), Author: f[2], Email: f[3], Date: f[4], Subject: f[5]}
		if c.Parents == nil {
			c.Parents = []string{}
		}
		commits = append(commits, c)
	}
	return commits
}

func isGitattributes(p string) bool {
	return p == ".gitattributes" || strings.HasSuffix(p, "/.gitattributes")
}

// checkLFS looks for filter=lfs in every .gitattributes of the tree, except
// under a link, and in info/attributes.
func (r *Inspection) checkLFS(root, commonDir string, idx *index, links map[string]bool) error {
	var found []string
	seen := map[string]bool{}
	check := func(base, rel, full string) error {
		if seen[full] {
			return nil
		}
		seen[full] = true
		if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, _, err := readUnder(base, full, maxFileSize)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errLink) {
			return nil
		}
		if err != nil {
			return err
		}
		if usesLFS(data) {
			found = append(found, rel)
		}
		return nil
	}
	var paths []string
	for _, e := range idx.entries {
		if e.stage == 0 && isGitattributes(e.path) {
			paths = append(paths, e.path)
		}
	}
	for _, p := range r.Status.Untracked {
		if isGitattributes(p) {
			paths = append(paths, p)
		}
	}
	for _, p := range paths {
		if underLink(links, p) {
			continue
		}
		if err := check(root, p, filepath.Join(root, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	info := filepath.Join(commonDir, "info", "attributes")
	if err := check(commonDir, info, info); err != nil {
		return err
	}
	if len(found) > 0 {
		r.block(BlockLFS, found...)
	}
	return nil
}

func usesLFS(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, attr := range fields[1:] {
			if attr == "filter=lfs" {
				return true
			}
		}
	}
	return false
}

// checkLinks finds symlinks and reparse points on the paths git reported, or
// tracked as links, whose targets leave the work tree. It returns every link
// prefix it found, so nothing is read beneath one.
func (r *Inspection) checkLinks(root string, idx *index) map[string]bool {
	var paths []string
	for _, e := range idx.entries {
		if e.stage == 0 && e.mode&modeTypeMask == modeSymlink {
			paths = append(paths, e.path)
		}
	}
	for _, c := range append(append([]Change{}, r.Status.Staged...), r.Status.Unstaged...) {
		paths = append(paths, c.Path)
	}
	paths = append(paths, r.Status.Untracked...)
	checked, links := map[string]bool{}, map[string]bool{}
	var escaping []string
	for _, p := range paths {
		parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
		for i := 1; i <= len(parts); i++ {
			prefix := strings.Join(parts[:i], "/")
			if checked[prefix] {
				if links[prefix] {
					break
				}
				continue
			}
			checked[prefix] = true
			full := filepath.Join(root, filepath.FromSlash(prefix))
			info, err := os.Lstat(full)
			if err != nil {
				break
			}
			if isLink(full, info) {
				links[prefix] = true
				if linkEscapes(root, full) {
					escaping = append(escaping, prefix)
				}
				break
			}
		}
	}
	if len(escaping) > 0 {
		sort.Strings(escaping)
		r.block(BlockEscapingLink, escaping...)
	}
	return links
}

// underLink reports a path at or beneath a link checkLinks found.
func underLink(links map[string]bool, p string) bool {
	for prefix := p; ; {
		if links[prefix] {
			return true
		}
		i := strings.LastIndex(prefix, "/")
		if i < 0 {
			return false
		}
		prefix = prefix[:i]
	}
}

// linkEscapes reports a link whose target leaves the work tree: a target
// outside it is never resolved, and one inside is followed with Lstat.
func linkEscapes(root, full string) bool {
	_, ok := resolve(root, full, true, true)
	return !ok
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var instructionNames = map[string]bool{
	"AGENTS.md": true, "CLAUDE.md": true, "GEMINI.md": true, "CONTRIBUTING.md": true, ".cursorrules": true,
	"go.mod": true, "package.json": true, "Cargo.toml": true, "pyproject.toml": true, "requirements.txt": true,
	"setup.py": true, "pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "Gemfile": true,
	"composer.json": true, "Makefile": true, "CMakeLists.txt": true, "deno.json": true, "mix.exs": true,
}

// instructionFiles lists the instruction and manifest files at the root.
func instructionFiles(root string) []string {
	files := []string{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return files
	}
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && (instructionNames[name] || strings.HasPrefix(strings.ToLower(name), "readme")) {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files
}

// fingerprint hashes HEAD, the index entries without their stat data, and
// each changed or untracked path's mode, size and content as Lstat sees it.
func fingerprint(root, head string, idx *index, st Status, links map[string]bool) string {
	h := sha256.New()
	field := func(parts ...string) {
		for _, p := range parts {
			var n [binary.MaxVarintLen64]byte
			h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
			io.WriteString(h, p)
		}
	}
	field("head", head)
	for _, e := range idx.entries {
		flags := fmt.Sprintf("%t,%t,%t", e.assumeValid, e.skipWork, e.intentToAdd)
		field("entry", e.path, strconv.FormatUint(uint64(e.mode), 8), hex.EncodeToString(e.oid), strconv.Itoa(e.stage), flags)
	}
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, c := range append(append([]Change{}, st.Staged...), st.Unstaged...) {
		add(c.Path)
	}
	for _, p := range append(append([]string{}, st.Untracked...), st.Unmerged...) {
		add(p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		state := []string{"under a link"}
		if !underLink(links, path.Dir(p)) {
			state = pathState(filepath.Join(root, filepath.FromSlash(p)))
		}
		field(append([]string{"path", p}, state...)...)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func pathState(full string) []string {
	info, err := os.Lstat(full)
	if err != nil {
		return []string{"absent"}
	}
	switch {
	case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
		target, _ := os.Readlink(full)
		return []string{"link", target}
	case info.IsDir():
		return []string{"dir"}
	case !info.Mode().IsRegular():
		return []string{"other"}
	}
	f, err := openFile(full)
	if err != nil {
		return []string{"unreadable"}
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !os.SameFile(info, now) {
		return []string{"unstable"}
	}
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return []string{"unreadable"}
	}
	mode := "file"
	if info.Mode().Perm()&0o100 != 0 {
		mode = "exec"
	}
	return []string{mode, strconv.FormatInt(n, 10), hex.EncodeToString(sum.Sum(nil))}
}
