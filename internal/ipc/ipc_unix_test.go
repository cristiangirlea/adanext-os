//go:build !windows

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSocketAndDirectoryBelongOnlyToTheOwner(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
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
	t.Setenv("XDG_RUNTIME_DIR", "")
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
	t.Setenv("XDG_RUNTIME_DIR", "")
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
	t.Setenv("XDG_RUNTIME_DIR", "")
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
	t.Setenv("XDG_RUNTIME_DIR", "")
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
