package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// openFile is how every user file is opened; it refuses anything but a
// regular file reached through no final link. Tests record it.
var openFile = openShared

// Read caps: over one is an inspection error.
const (
	maxIndexSize = 256 << 20
	maxFileSize  = 16 << 20
)

var (
	errLink       = errors.New("a link or reparse point")
	errNotRegular = errors.New("not a regular file")
	errRemote     = errors.New("a remote path")
)

// readFile reads a regular file of at most limit bytes, holding its handle
// only for the read.
func readFile(path string, limit int64) ([]byte, os.FileInfo, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	return data, info, err
}

// readUnder reads path, refusing a link at any component below base.
func readUnder(base, path string, limit int64) ([]byte, os.FileInfo, error) {
	if err := noLinks(base, path); err != nil {
		return nil, nil, err
	}
	return readFile(path, limit)
}

// noLinks Lstats each component of path below base and refuses a link or
// reparse point; a missing component is fs.ErrNotExist.
func noLinks(base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil || !within(base, path) {
		return fmt.Errorf("%s is not under %s", path, base)
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if isLink(cur, info) {
			return &os.PathError{Op: "open", Path: cur, Err: errLink}
		}
	}
	return nil
}

// linkTarget makes a link's target absolute; ok is false for a remote,
// rooted or drive-relative one. On Unix it keeps its `..`, which resolve
// takes after the links before it; Windows normalises lexically.
func linkTarget(link, target string) (string, bool) {
	if runtime.GOOS == "windows" {
		for _, prefix := range []string{`\\?\`, `\??\`} {
			if rest, ok := strings.CutPrefix(target, prefix); ok && filepath.VolumeName(rest) != "" && !strings.HasPrefix(rest, `UNC\`) {
				target = rest
			}
		}
	}
	if IsRemotePath(target) {
		return "", false
	}
	if !filepath.IsAbs(target) {
		if filepath.VolumeName(target) != "" || strings.HasPrefix(target, `\`) || strings.HasPrefix(target, "/") {
			return "", false
		}
		target = filepath.Dir(link) + string(filepath.Separator) + target
	}
	if runtime.GOOS == "windows" {
		target = filepath.Clean(target)
	}
	return target, true
}

func volumeRoot(p string) string { return filepath.VolumeName(p) + string(filepath.Separator) }

// resolve follows the links in p, under root, one component at a time with
// Lstat and never through a remote target. ok is false at an unreadable
// symlink, a remote or rooted target, a loop, a target outside root when
// bounded (a `..` above it too), or any link at all when follow is false.
func resolve(root, p string, bounded, follow bool) (string, bool) {
	parts := func(root, p string) []string {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return nil
		}
		return strings.Split(rel, string(filepath.Separator))
	}
	sep := string(filepath.Separator)
	rest, cur := parts(root, p), root
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			// Taken from the resolved directory, as the kernel does.
			if bounded && !within(root, filepath.Dir(cur)) {
				return "", false
			}
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		info, err := os.Lstat(next)
		if err != nil {
			// A later `..` would be taken lexically, through links never resolved.
			if slices.Contains(rest, "..") {
				return "", false
			}
			return filepath.Join(append([]string{next}, rest...)...), true
		}
		if !isLink(next, info) {
			cur = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || !follow || hops > 40 {
			return "", false
		}
		abs, ok := linkTarget(next, target)
		if !ok {
			return "", false
		}
		// The target is walked from its components before its first `..`.
		c := strings.Split(abs, sep)
		i := 0
		for i < len(c) && c[i] != ".." {
			i++
		}
		head := filepath.Clean(strings.Join(c[:i], sep) + sep)
		if !bounded {
			root = volumeRoot(head)
		} else if !within(root, head) {
			return "", false
		}
		rest, cur = append(append(parts(root, head), c[i:]...), rest...), root
	}
	return cur, true
}

// localPath resolves a path named by the user's own config, following its
// links (when follow is set) but never to a remote path.
func localPath(p string, follow bool) (string, bool) {
	if IsRemotePath(p) || !filepath.IsAbs(p) {
		return "", false
	}
	return resolve(volumeRoot(p), filepath.Clean(p), false, follow)
}

// readUserFile reads a system or global config file, following its local links.
func readUserFile(path string) ([]byte, error) {
	resolved, ok := localPath(path, true)
	if !ok {
		return nil, fmt.Errorf("%s: a remote or unresolvable link", path)
	}
	data, _, err := readFile(resolved, maxFileSize)
	return data, err
}

// IsRemotePath reports a UNC or device path (\\host\, \\?\, \\.\ and their
// forward-slash forms), which is refused unopened: opening one sends the
// user's credentials over SMB.
func IsRemotePath(p string) bool {
	p = strings.ReplaceAll(p, `/`, `\`)
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, `\??\`)
}

type layout struct {
	workTree, gitDir, commonDir string
	alternates                  []string // the original's alternates chain, absolute
}

// readPointer reads a one-line pointer file such as .git or commondir.
func readPointer(base, path string) (string, error) {
	data, _, err := readUnder(base, path, maxFileSize)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// resolvePointer makes a pointer absolute against base and resolves the links
// of every component, refusing a remote one, so no later Lstat crosses a link
// to a remote target. It returns the named path too.
func resolvePointer(base, value string) (resolved, named string, b *Blocker) {
	if IsRemotePath(value) {
		return "", "", &Blocker{Code: BlockRemotePath, Paths: []string{value}}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, filepath.FromSlash(value))
	}
	named = filepath.Clean(value)
	resolved, ok := localPath(named, true)
	if !ok {
		return "", "", &Blocker{Code: BlockRemotePath, Paths: []string{named}}
	}
	return resolved, named, nil
}

// readLayout finds the git directory of the work tree root and its common
// directory and alternates, reading only files whose paths are local.
func readLayout(root string) (layout, *Blocker) {
	l := layout{workTree: root}
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if isGitDir(root) {
			return l, &Blocker{Code: BlockBare, Paths: []string{root}}
		}
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{root}}
	case err != nil:
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{dotGit}}
	case info.IsDir():
		l.gitDir = dotGit
	case info.Mode().IsRegular():
		line, err := readPointer(root, dotGit)
		value, ok := strings.CutPrefix(line, "gitdir: ")
		if err != nil || !ok || value == "" {
			return l, &Blocker{Code: BlockNotRepository, Paths: []string{dotGit}}
		}
		var b *Blocker
		if l.gitDir, _, b = resolvePointer(root, value); b != nil {
			return l, b
		}
	default:
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{dotGit}}
	}
	if !isGitDir(l.gitDir) && !isLinkedGitDir(l.gitDir) {
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{l.gitDir}}
	}
	l.commonDir = l.gitDir
	if value, err := readPointer(l.gitDir, filepath.Join(l.gitDir, "commondir")); err == nil {
		var b *Blocker
		if l.commonDir, _, b = resolvePointer(l.gitDir, value); b != nil {
			return l, b
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{filepath.Join(l.gitDir, "commondir")}}
	}
	if !isGitDir(l.commonDir) {
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{l.commonDir}}
	}
	return l, l.readAlternates(filepath.Join(l.commonDir, "objects"), 0)
}

// readAlternates follows objects/info/alternates as git does, to its depth limit.
func (l *layout) readAlternates(objects string, depth int) *Blocker {
	if depth > 5 {
		return nil
	}
	file := filepath.Join(objects, "info", "alternates")
	data, _, err := readUnder(objects, file, maxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &Blocker{Code: BlockNotRepository, Paths: []string{file}}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.Trim(line, `"`)
		alt, named, b := resolvePointer(objects, line)
		if b != nil {
			return b
		}
		// An alternate that is itself a link is refused; the links above it are local.
		if info, err := os.Lstat(named); err == nil && isLink(named, info) {
			return &Blocker{Code: BlockNotRepository, Paths: []string{named}}
		}
		l.alternates = append(l.alternates, alt)
		if b := l.readAlternates(alt, depth+1); b != nil {
			return b
		}
	}
	return nil
}

func isGitDir(dir string) bool {
	head, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		if info, err := os.Lstat(filepath.Join(dir, sub)); err != nil || !info.IsDir() || isLink(filepath.Join(dir, sub), info) {
			return false
		}
	}
	return !linkedDir(dir, "info")
}

// linkedDir reports a git directory's own directory that is a link.
func linkedDir(dir string, subs ...string) bool {
	for _, sub := range subs {
		if info, err := os.Lstat(filepath.Join(dir, sub)); err == nil && isLink(filepath.Join(dir, sub), info) {
			return true
		}
	}
	return false
}

// isLinkedGitDir is a worktree's git directory: HEAD and a commondir file.
func isLinkedGitDir(dir string) bool {
	_, head := os.Lstat(filepath.Join(dir, "HEAD"))
	_, common := os.Lstat(filepath.Join(dir, "commondir"))
	return head == nil && common == nil && !linkedDir(dir, "refs", "info")
}
