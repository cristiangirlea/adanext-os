//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Another account that could replace the token could also replace the socket: a
// client reads the token only from the owner's own 0700 directory.
func TestDialRefusesATokenDirectoryThatIsNotOwnerOnly(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "token-dir"}
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

// A directory the user chose, 0755 say, may be their home or a project: Acquire refuses it
// with the command that fixes it, and leaves its mode alone.
func TestAcquireRefusesAStateDirectoryThatIsNotOwnerOnlyAndLeavesItsMode(t *testing.T) {
	cfg := Config{StateDir: filepath.Join(ownerOnlyDir(t), "state"), Name: "token-dir"}
	if err := os.Mkdir(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if lock, err := Acquire(cfg); err == nil {
		lock.Release()
		t.Fatal("Acquire accepted a 0755 state directory")
	} else if !strings.Contains(err.Error(), fmt.Sprintf("run chmod 700 %q", cfg.StateDir)) {
		t.Fatalf("Acquire = %v", err)
	}
	if info, err := os.Lstat(cfg.StateDir); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("state directory = %v %v", info.Mode(), err)
	}
}

// The default directory is made 0700, and Acquire takes it as it is.
func TestAcquireTakesAStateDirectoryMadeOwnerOnly(t *testing.T) {
	cfg := Config{StateDir: filepath.Join(ownerOnlyDir(t), "a", "state"), Name: "token-dir"}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
}

// The token is opened without following a link or waiting on a FIFO and checked as opened:
// an account that swapped the directory could make none of them a 0600 file the user owns.
func TestDialRefusesATokenThatIsALinkAFIFOOrNotOwnerOnly(t *testing.T) {
	for _, kind := range []string{"link", "FIFO", "mode 0640"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{StateDir: ownerOnlyDir(t), Name: "token-file"}
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
			token := filepath.Join(cfg.StateDir, "adad.token")
			if kind == "link" {
				key, err := os.ReadFile(token)
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(ownerOnlyDir(t), "key")
				if err := os.WriteFile(other, key, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(token); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, token); err != nil {
					t.Fatal(err)
				}
			} else if kind == "FIFO" {
				if err := os.Remove(token); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(token, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(token, 0640); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			dialed := make(chan error, 1)
			go func() {
				c, err := Dial(ctx, cfg)
				if c != nil {
					c.Close()
				}
				dialed <- err
			}()
			select {
			case err := <-dialed:
				if err == nil {
					t.Fatal("Dial trusted the token")
				}
			case <-time.After(3 * time.Second):
				// A writer releases the blocked open, so the test ends.
				if w, err := os.OpenFile(token, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					w.Close()
				}
				t.Fatal("Dial waited on the token")
			}
		})
	}
}

func TestListenRefusesToPublishATokenWhereClientsWouldRefuseIt(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "token-dir"}
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
