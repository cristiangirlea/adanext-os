//go:build !windows

package artifact

import (
	"context"
	"errors"
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

func TestASymlinkBlobIsRefusedByOpenAndReuse(t *testing.T) {
	s := testStore(t)
	data := []byte("private")
	ref, err := s.Write(context.Background(), strings.NewReader(string(data)), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if reader, err := s.Open(ref.Hash); !errors.Is(err, ErrCorrupt) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("open symlink: %v", err)
	}
	if _, err := s.Write(context.Background(), strings.NewReader(string(data)), 100); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("reuse symlink: %v", err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains")
	}
}

func TestOpenTightensExistingOwnedDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, tmp} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, tmp} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s: %v %v", path, info, err)
		}
	}
}

func TestAnotherUsersDirectoryIsRefusedWithoutChmod(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherUID := uint32(os.Geteuid()) + 1
	if err := makePrivateDirectoryForUID(dir, otherUID); err == nil {
		t.Fatal("accepted another user's directory")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("another user's directory was chmodded: %v %v", info, err)
	}
}

func TestADirectorySwappedForASymlinkBetweenCheckAndChmodIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	target := filepath.Join(t.TempDir(), "target")
	for _, path := range []string{dir, target} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	swapped := false
	beforeDirectoryChmod = func(path string) {
		if path != dir || swapped {
			return
		}
		swapped = true
		if err := os.Rename(dir, dir+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDirectoryChmod = nil })
	if _, err := Open(dir); err == nil {
		t.Fatal("accepted a directory swapped for a symlink")
	}
	if !swapped {
		t.Fatal("the swap hook did not run")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("symlink target was chmodded: %v %v", info, err)
	}
}
