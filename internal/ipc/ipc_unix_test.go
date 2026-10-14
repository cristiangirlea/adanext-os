//go:build linux

package ipc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestSocketAndDirectoryBelongOnlyToTheOwner(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "permissions"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(cfg.StateDir, "run"), 0700},
		{filepath.Join(cfg.StateDir, "run", cfg.Name+".sock"), 0600},
	} {
		info, err := os.Lstat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Fatalf("%s mode=%o, want %o", tc.path, info.Mode().Perm(), tc.mode)
		}
		if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			t.Fatalf("%s has another owner", tc.path)
		}
	}
}

func TestUnsafeSocketDirectoryIsRefusedByListenAndDial(t *testing.T) {
	for _, kind := range []string{"wide mode", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir(), Name: "unsafe"}
			path := filepath.Join(cfg.StateDir, "run")
			if kind == "wide mode" {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := Acquire(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			if l, err := lock.Listen(); err == nil {
				l.Close()
				t.Fatal("Listen accepted unsafe directory")
			}
			if c, err := Dial(context.Background(), cfg); err == nil {
				c.Close()
				t.Fatal("Dial accepted unsafe directory")
			}
		})
	}
}

func TestPeerWithAnotherUIDIsRejected(t *testing.T) {
	if allowPeerUID(uint32(os.Getuid() + 1)) {
		t.Fatal("another UID was allowed")
	}
	if !allowPeerUID(uint32(os.Getuid())) {
		t.Fatal("current UID was rejected")
	}
}

func TestSocketDirectoryOwnedByAnotherUserIsRefused(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing directory ownership requires root")
	}
	cfg := Config{StateDir: t.TempDir(), Name: "foreign"}
	dir := filepath.Join(cfg.StateDir, "run")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 1, -1); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if l, err := lock.Listen(); err == nil {
		l.Close()
		t.Fatal("Listen accepted another owner")
	}
	if c, err := Dial(context.Background(), cfg); err == nil {
		c.Close()
		t.Fatal("Dial accepted another owner")
	}
}

func TestListenReplacesAnUnservedStaleSocket(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "stale"}
	dir := filepath.Join(cfg.StateDir, "run")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cfg.Name+".sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	l, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
}

func TestListenRefusesANonSocketEndpoint(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "occupied"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	dir := filepath.Join(cfg.StateDir, "run")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cfg.Name+".sock"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := lock.Listen(); err == nil {
		listener.Close()
		t.Fatal("Listen replaced a non-socket endpoint")
	}
}

func TestDifferentStateDirsWithSameNameKeepBothSockets(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	configs := []Config{
		{StateDir: t.TempDir(), Name: "shared"},
		{StateDir: t.TempDir(), Name: "shared"},
	}
	for i, cfg := range configs {
		lock, err := Acquire(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		listener, err := lock.Listen()
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func(want byte) {
			conn, err := listener.Accept()
			if err == nil {
				defer conn.Close()
				_, _ = conn.Write([]byte{want})
			}
		}(byte('A' + i))
	}
	for i, cfg := range configs {
		conn, err := Dial(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Dial(%s): %v", cfg.StateDir, err)
		}
		var got [1]byte
		if _, err := io.ReadFull(conn, got[:]); err != nil {
			t.Fatal(err)
		}
		if want := byte('A' + i); got[0] != want {
			t.Fatalf("Dial(%s) reached %q, want %q", cfg.StateDir, got[0], want)
		}
		conn.Close()
	}
}

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

func TestListenAndDialNameAUnixSocketPathThatIsTooLong(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peer checks are unsupported on this Unix system")
	}
	cfg := Config{StateDir: filepath.Join(t.TempDir(), strings.Repeat("a", 108)), Name: "long"}
	path := filepath.Join(cfg.StateDir, "run", cfg.Name+".sock")
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if listener, err := listen(cfg); err == nil || !strings.Contains(err.Error(), path) {
		if listener != nil {
			listener.Close()
		}
		t.Fatalf("Listen error = %v, want path %q", err, path)
	}
	if conn, err := Dial(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), path) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("Dial error = %v, want path %q", err, path)
	}
}

func TestDialRefusesAServerWhenTheExpectedUIDChanges(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peer checks are unsupported on this Unix system")
	}
	cfg := Config{StateDir: t.TempDir(), Name: "uid-check"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	old := currentPeerUID
	currentPeerUID = func() uint32 { return uint32(os.Getuid() + 1) }
	defer func() { currentPeerUID = old }()
	conn, err := Dial(context.Background(), cfg)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "different user") {
		t.Fatalf("Dial accepted a mismatched server UID: %v", err)
	}
}

func TestXDGRuntimeDirDoesNotChangeSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := Config{StateDir: t.TempDir(), Name: "location"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	want := filepath.Join(cfg.StateDir, "run", cfg.Name+".sock")
	if listener.Addr().String() != want {
		t.Fatalf("socket path = %q, want %q", listener.Addr(), want)
	}
}

func TestCloseDoesNotRemoveReplacementSocket(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "replacement"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	first, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	path := first.Addr().String()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("replacement socket removed: %v", err)
	}
	if !os.SameFile(secondInfo, currentInfo) {
		t.Fatal("replacement socket changed")
	}
}

func TestCloseRemovesItsSocketWhileTheListenerIsOpen(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "close-order"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	owner := listener.(*ownerListener)
	defer owner.Close()
	owner.removePath = func(path string) error {
		raw, err := owner.UnixListener.SyscallConn()
		if err != nil {
			return fmt.Errorf("listener was closed before socket removal: %w", err)
		}
		if err := raw.Control(func(uintptr) {}); err != nil {
			return fmt.Errorf("listener was closed before socket removal: %w", err)
		}
		return os.Remove(path)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}
