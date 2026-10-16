package source

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// openFile is how every user file is opened; tests record it.
var openFile = openShared

// readFile reads a user's file, holding its handle only for the read.
func readFile(path string) ([]byte, os.FileInfo, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	return data, info, err
}

// isRemotePath reports a UNC or device path (\\host\, \\?\, \\.\ and their
// forward-slash forms), which is refused unopened: opening one sends the
// user's credentials over SMB.
func isRemotePath(p string) bool {
	p = strings.ReplaceAll(p, `/`, `\`)
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, `\??\`)
}

type layout struct {
	workTree, gitDir, commonDir string
	alternates                  []string // the original's alternates chain, absolute
}

// readPointer reads a one-line pointer file such as .git or commondir.
func readPointer(path string) (string, error) {
	data, _, err := readFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// resolvePointer makes a pointer absolute against base, refusing a remote one.
func resolvePointer(base, value string) (string, *Blocker) {
	if isRemotePath(value) {
		return "", &Blocker{Code: BlockRemotePath, Paths: []string{value}}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, filepath.FromSlash(value))
	}
	return filepath.Clean(value), nil
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
		line, err := readPointer(dotGit)
		value, ok := strings.CutPrefix(line, "gitdir: ")
		if err != nil || !ok || value == "" {
			return l, &Blocker{Code: BlockNotRepository, Paths: []string{dotGit}}
		}
		var b *Blocker
		if l.gitDir, b = resolvePointer(root, value); b != nil {
			return l, b
		}
	default:
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{dotGit}}
	}
	if !isGitDir(l.gitDir) && !isLinkedGitDir(l.gitDir) {
		return l, &Blocker{Code: BlockNotRepository, Paths: []string{l.gitDir}}
	}
	l.commonDir = l.gitDir
	if value, err := readPointer(filepath.Join(l.gitDir, "commondir")); err == nil {
		var b *Blocker
		if l.commonDir, b = resolvePointer(l.gitDir, value); b != nil {
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
	data, _, err := readFile(filepath.Join(objects, "info", "alternates"))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.Trim(line, `"`)
		alt, b := resolvePointer(objects, line)
		if b != nil {
			return b
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
		if info, err := os.Stat(filepath.Join(dir, sub)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// isLinkedGitDir is a worktree's git directory: HEAD and a commondir file.
func isLinkedGitDir(dir string) bool {
	_, head := os.Lstat(filepath.Join(dir, "HEAD"))
	_, common := os.Lstat(filepath.Join(dir, "commondir"))
	return head == nil && common == nil
}
