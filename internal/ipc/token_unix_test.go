//go:build !windows

package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Another account that could replace the token could also replace the socket: a
// client reads the token only from the owner's own 0700 directory.
func TestDialRefusesATokenDirectoryThatIsNotOwnerOnly(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "token-dir"}
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
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := os.Chmod(cfg.StateDir, 0750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, cfg)
	if c != nil {
		c.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("Dial = %v", err)
	}
}

// A directory the user made, 0755 say, is the user's own and not writable by others:
// Acquire keeps it, made owner-only, so the coordinator is reachable.
func TestAcquireMakesTheUsersOwnStateDirectoryOwnerOnly(t *testing.T) {
	cfg := Config{StateDir: filepath.Join(t.TempDir(), "state"), Name: "token-dir"}
	if err := os.Mkdir(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	info, err := os.Lstat(cfg.StateDir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("state directory = %v %v", info.Mode(), err)
	}
}

func TestListenRefusesToPublishATokenWhereClientsWouldRefuseIt(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "token-dir"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := os.Chmod(cfg.StateDir, 0750); err != nil {
		t.Fatal(err)
	}
	if l, err := lock.Listen(); err == nil {
		l.Close()
		t.Fatal("Listen published a token in a 0750 directory")
	}
}
