//go:build !windows

package ipc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestTheLockOpensInTheCheckedDirectoryWhenItsPathBecomesASymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	elsewhere := t.TempDir()
	afterStateDirChecked = func() {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterStateDirChecked = nil })
	lock, err := Acquire(Config{StateDir: dir, Name: "swap"})
	if err != nil {
		t.Fatalf("Acquire did not keep the checked directory: %v", err)
	}
	defer lock.Release()
	if _, err := os.Lstat(filepath.Join(elsewhere, "adad.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file followed the swapped path: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "adad.lock")); err != nil {
		t.Fatalf("lock file is not in the checked directory: %v", err)
	}
}

func TestAnExistingLockOpensInTheCheckedDirectoryWhenItsPathBecomesASymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "adad.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	elsewhere := t.TempDir()
	afterStateDirChecked = func() {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterStateDirChecked = nil })
	lock, err := Acquire(Config{StateDir: dir, Name: "swap-existing"})
	if err != nil {
		t.Fatalf("Acquire did not keep the checked directory: %v", err)
	}
	defer lock.Release()
	if _, err := os.Lstat(filepath.Join(elsewhere, "adad.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file followed the swapped path: %v", err)
	}
	if record := readLockRecord(lock.file); record.PID != os.Getpid() {
		t.Fatalf("lock record pid = %d, want %d in the checked directory", record.PID, os.Getpid())
	}
	got, err := os.ReadFile(filepath.Join(moved, "adad.lock"))
	if err != nil || !strings.Contains(string(got), `"pid"`) {
		t.Fatalf("existing lock in the checked directory was not written: %q %v", got, err)
	}
}

func TestTheCheckedDirectoryIsClosedWhenAcquireIsAbandonedAfterTheCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts descriptors through /proc/self/fd")
	}
	cfg := Config{StateDir: t.TempDir(), Name: "abandon"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	openDescriptors := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := openDescriptors()
	afterStateDirChecked = func() { panic("abandon Acquire") }
	t.Cleanup(func() { afterStateDirChecked = nil })
	var abandoned *Lock
	func() {
		defer func() { _ = recover() }()
		abandoned, _ = Acquire(cfg)
	}()
	if abandoned != nil {
		abandoned.Release()
		t.Fatal("Acquire returned a lock after the hook abandoned it")
	}
	if after := openDescriptors(); after != before {
		t.Fatalf("open descriptors = %d after abandoning Acquire, want %d", after, before)
	}
}

func TestAStateDirectorySymlinkIsRefusedOnUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(Config{StateDir: path, Name: "link"})
	if lock != nil {
		lock.Release()
	}
	if err == nil || !strings.Contains(err.Error(), "must be a real directory") {
		t.Fatalf("Acquire error = %v, want the state directory check", err)
	}
}
