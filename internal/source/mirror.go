package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// mirrorKeys are the data-only keys copied into the mirror's config: they
// change how git reads the work tree, and none of them names a command.
var mirrorKeys = []string{
	"core.filemode", "core.symlinks", "core.ignorecase", "core.precomposeunicode",
	"core.autocrlf", "core.eol", "core.safecrlf", "core.checkRoundtripEncoding",
	"core.checkStat", "core.trustctime", "core.longpaths",
}

func quoteConfig(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(v) + `"`
}

// mirrorSource is what the mirror is built from, all read in Go beforehand.
type mirrorSource struct {
	layout       layout
	repo         *config // repository config, for the format version and object format
	merged       *config // system, global and repository config in order
	files        ConfigFiles
	index        []byte // nil when the repository has no index yet
	indexModTime time.Time
	head         string // the HEAD commit, empty when unborn
}

// writeMirror builds a git directory at mirror that git can use with the
// original's work tree: Ada's config, copies of HEAD, refs, shallow, the
// excludes and attributes, the index with its mtime, the original's objects
// through alternates, and refs/ada/head naming the HEAD commit, so a fetch
// always reaches a detached HEAD.
func writeMirror(mirror string, src mirrorSource) error {
	for _, dir := range []string{"objects/info", "refs", "info"} {
		if err := os.MkdirAll(filepath.Join(mirror, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	l := src.layout
	if err := writeAdaConfig(mirror, src, false, ""); err != nil {
		return err
	}
	if copied, err := copyRepoFile(l.gitDir, filepath.Join(l.gitDir, "HEAD"), filepath.Join(mirror, "HEAD")); err != nil || !copied {
		if err == nil {
			err = errors.New("HEAD vanished")
		}
		return err
	}
	for _, name := range []string{"packed-refs", "shallow", "info/exclude", "info/attributes"} {
		if _, err := copyRepoFile(l.commonDir, filepath.Join(l.commonDir, filepath.FromSlash(name)), filepath.Join(mirror, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	if err := copyTree(filepath.Join(l.commonDir, "refs"), filepath.Join(mirror, "refs")); err != nil {
		return err
	}
	if l.gitDir != l.commonDir {
		// A linked worktree's own refs (bisect, worktree, rewritten) live in its git directory.
		if err := copyTree(filepath.Join(l.gitDir, "refs"), filepath.Join(mirror, "refs")); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(mirror, "refs", "ada")); err != nil {
		return err
	}
	if src.head != "" {
		if err := os.MkdirAll(filepath.Join(mirror, "refs", "ada"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mirror, "refs", "ada", "head"), []byte(src.head+"\n"), 0o600); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(filepath.Join(l.commonDir, "objects")); err != nil || !info.IsDir() || isLink(filepath.Join(l.commonDir, "objects"), info) {
		return errors.New("objects is not a directory")
	}
	objects := filepath.ToSlash(filepath.Join(l.commonDir, "objects"))
	if err := os.WriteFile(filepath.Join(mirror, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return err
	}
	if src.index != nil {
		path := filepath.Join(mirror, "index")
		if err := os.WriteFile(path, src.index, 0o600); err != nil {
			return err
		}
		// A newer mtime would hide racily clean edits from git.
		if err := os.Chtimes(path, src.indexModTime, src.indexModTime); err != nil {
			return err
		}
	}
	return nil
}

// writeAdaConfig writes the config of Ada's git directory dir: the format
// version and object format, the data-only keys, and copies of the user's
// excludes and attributes files; core holds further [core] lines, and
// sections further sections.
func writeAdaConfig(dir string, src mirrorSource, bare bool, sections string, core ...string) error {
	var cfg strings.Builder
	version, _ := src.repo.get("core.repositoryformatversion")
	if version == "" {
		version = "0"
	}
	cfg.WriteString("[core]\n\trepositoryformatversion = " + quoteConfig(version) + "\n\tbare = " + strconv.FormatBool(bare) + "\n")
	for _, line := range core {
		cfg.WriteString("\t" + line + "\n")
	}
	for _, key := range mirrorKeys {
		if v, ok := src.merged.get(key); ok {
			cfg.WriteString("\t" + key[len("core."):] + " = " + quoteConfig(v) + "\n")
		}
	}
	for _, f := range []struct{ key, fallback, name string }{
		{"core.excludesFile", "ignore", "ada-excludes"},
		{"core.attributesFile", "attributes", "ada-attributes"},
	} {
		dst := filepath.Join(dir, f.name)
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		path := src.files.userFile(src.merged, f.key, f.fallback)
		if path == "" || IsRemotePath(path) {
			continue
		}
		copied, err := copyUserFile(path, dst)
		if err != nil {
			return err
		}
		if copied {
			cfg.WriteString("\t" + f.key[len("core."):] + " = " + quoteConfig(filepath.ToSlash(dst)) + "\n")
		}
	}
	if format, ok := src.repo.get("extensions.objectformat"); ok {
		cfg.WriteString("[extensions]\n\tobjectformat = " + quoteConfig(format) + "\n")
	}
	cfg.WriteString(sections)
	return os.WriteFile(filepath.Join(dir, "config"), []byte(cfg.String()), 0o600)
}

// copyUserFile copies a regular file a config names, following its local
// links; a missing, non-regular or remote one is not copied.
func copyUserFile(src, dst string) (bool, error) {
	resolved, ok := localPath(src, true)
	if !ok {
		return false, nil
	}
	if info, err := os.Lstat(resolved); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	} else if !info.Mode().IsRegular() {
		return false, nil
	}
	data, _, err := readFile(resolved, maxFileSize)
	return writeCopy(dst, data, err)
}

// copyRepoFile copies a file of the git directory base, read through no link;
// a missing one is not copied.
func copyRepoFile(base, src, dst string) (bool, error) {
	data, _, err := readUnder(base, src, maxFileSize)
	return writeCopy(dst, data, err)
}

func writeCopy(dst string, data []byte, err error) (bool, error) {
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, data, 0o600)
}

// copyTree copies the regular files under src, following no links.
func copyTree(src, dst string) error {
	base := filepath.Dir(src)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			_, err := copyRepoFile(base, path, target)
			return err
		}
		return nil
	})
}
