//go:build !windows

package ipc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestALockSymlinkIsRefusedWithoutChangingItsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "adad.lock")); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(Config{StateDir: dir, Name: "symlink"})
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("Acquire followed a lock symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "keep" {
		t.Fatalf("target changed: %q %v", got, err)
	}
}

func TestAGroupWritableStateDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(Config{StateDir: dir, Name: "wide"})
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("Acquire accepted a 0777 state directory")
	}
}

func TestAGroupReadableLockFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adad.lock")
	if err := os.WriteFile(path, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(Config{StateDir: dir, Name: "wide-lock"})
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("Acquire accepted a group-readable lock file")
	}
}
