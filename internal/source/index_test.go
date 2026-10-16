package source

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lsFiles is what git itself reads from the index: mode, object, stage, path.
func lsFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, rec := range strings.Split(git(t, dir, "ls-files", "-s", "-z"), "\x00") {
		if rec != "" {
			out = append(out, rec)
		}
	}
	return out
}

func readFixtureIndex(t *testing.T, dir string, hashSize int) *index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	x, err := parseIndex(data, hashSize)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestTheIndexReaderAgreesWithGit(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.txt": "a\n", "dir/sub/long-name-one.txt": "1\n", "dir/sub/long-name-two.txt": "2\n", "dir/z": "z\n", "é.txt": "e\n"}
	cases := []struct {
		name     string
		init     []string
		version  string
		hashSize int
	}{
		{"version 2", nil, "2", 20},
		{"version 3", nil, "3", 20},
		{"version 4", nil, "4", 20},
		{"sha256", []string{"--object-format=sha256"}, "4", 32},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := tempDir(t)
			git(t, dir, append([]string{"init", "-q"}, c.init...)...)
			for name, content := range files {
				write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
			}
			git(t, dir, "add", "-A")
			git(t, dir, "update-index", "--index-version", c.version)
			if c.version != "2" {
				git(t, dir, "update-index", "--skip-worktree", "dir/z")
			}
			x := readFixtureIndex(t, dir, c.hashSize)
			if fmt.Sprint(x.version) != c.version {
				t.Fatalf("version = %d", x.version)
			}
			want := lsFiles(t, dir)
			if len(x.entries) != len(want) {
				t.Fatalf("%d entries, git lists %d", len(x.entries), len(want))
			}
			for i, e := range x.entries {
				got := fmt.Sprintf("%06o %s %d\t%s", e.mode, hex.EncodeToString(e.oid), e.stage, e.path)
				if got != want[i] {
					t.Errorf("entry %d = %q, git %q", i, got, want[i])
				}
				if e.skipWork != (c.version != "2" && e.path == "dir/z") {
					t.Errorf("%s skip-worktree = %v", e.path, e.skipWork)
				}
			}
		})
	}
}

func TestTheIndexReaderFindsStagesGitlinksAndExtensions(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"one/a.txt": "a\n", "two/b.txt": "b\n"})
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",mod")
	git(t, dir, "sparse-checkout", "set", "--cone", "--sparse-index", "one")
	x := readFixtureIndex(t, dir, 20)
	if !x.has("sdir") {
		t.Errorf("extensions = %v, want sdir", x.extensions)
	}
	var gitlink, sparseDir bool
	for _, e := range x.entries {
		gitlink = gitlink || e.path == "mod" && e.mode == modeGitlink
		sparseDir = sparseDir || e.mode&modeTypeMask == modeDir && e.skipWork
	}
	if !gitlink || !sparseDir {
		t.Errorf("gitlink %v, sparse directory %v in %+v", gitlink, sparseDir, x.entries)
	}

	split := newRepo(t, nil)
	git(t, split, "update-index", "--split-index")
	if x := readFixtureIndex(t, split, 20); !x.has("link") {
		t.Errorf("extensions = %v, want link", x.extensions)
	}
}

func TestATornIndexIsRefused(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	data, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	data[20] ^= 0xff
	if _, err := parseIndex(data, 20); !errors.Is(err, errIndexChecksum) {
		t.Fatalf("parse = %v", err)
	}
	if _, err := parseIndex(data[:30], 20); err == nil {
		t.Fatal("a truncated index parsed")
	}
}
