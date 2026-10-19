//go:build linux

package ipc

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSocketAndDirectoryBelongOnlyToTheOwner(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "permissions"}
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
			cfg := Config{StateDir: ownerOnlyDir(t), Name: "unsafe"}
			path := filepath.Join(cfg.StateDir, "run")
			if kind == "wide mode" {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(ownerOnlyDir(t), path); err != nil {
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "foreign"}
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "stale"}
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "occupied"}
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
	t.Setenv("XDG_RUNTIME_DIR", ownerOnlyDir(t))
	configs := []Config{
		{StateDir: ownerOnlyDir(t), Name: "shared"},
		{StateDir: ownerOnlyDir(t), Name: "shared"},
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

func TestListenAndDialNameAUnixSocketPathThatIsTooLong(t *testing.T) {
	cfg := Config{StateDir: filepath.Join(ownerOnlyDir(t), strings.Repeat("a", 108)), Name: "long"}
	path := filepath.Join(cfg.StateDir, "run", cfg.Name+".sock")
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if listener, err := listen(cfg); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "sun_path limit") {
		if listener != nil {
			listener.Close()
		}
		t.Fatalf("Listen error = %v, want sun_path limit and path %q", err, path)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "run")); !os.IsNotExist(err) {
		t.Fatalf("Listen created run directory for overlong path: %v", err)
	}
	if conn, err := Dial(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "sun_path limit") {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("Dial error = %v, want sun_path limit and path %q", err, path)
	}
}

func TestDialRefusesAServerWhenTheExpectedUIDChanges(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "uid-check"}
	// The listener's accept goroutine reads the seam: swap it before Listen
	// and restore it only after Close has waited for that goroutine.
	old := currentPeerUID
	currentPeerUID = func() uint32 { return uint32(os.Getuid() + 1) }
	defer func() { currentPeerUID = old }()
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
	conn, err := Dial(context.Background(), cfg)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "different user") {
		t.Fatalf("Dial accepted a mismatched server UID: %v", err)
	}
}

func TestXDGRuntimeDirDoesNotChangeSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", ownerOnlyDir(t))
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "location"}
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "replacement"}
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "close-order"}
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
	owner := listener.(*authenticatedListener).Listener.(*ownerListener)
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
