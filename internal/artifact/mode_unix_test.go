//go:build !windows

package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestFilesArePrivateAndASymlinkDirectoryIsRefused(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	s := testStore(t)
	ref, err := s.Write(context.Background(), strings.NewReader("private"), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.dir, filepath.Join(s.dir, "tmp"), filepath.Join(s.dir, ref.Hash[:2])} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s: %v %v", path, info, err)
		}
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(s.dir, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(link); err == nil {
		_ = opened
		t.Fatal("accepted symlink")
	}
}

func TestSymlinkedTemporaryDirectoryIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("accepted symlinked tmp directory")
	}
}
