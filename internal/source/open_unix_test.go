//go:build !windows

package source

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAConfigThatIsNotARegularFileFailsPromptly(t *testing.T) {
	t.Parallel()
	for name, create := range map[string]func(string) error{
		"a fifo":              func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"a link to /dev/zero": func(p string) error { return os.Symlink("/dev/zero", p) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t, nil)
			config := filepath.Join(dir, ".git", "config")
			if err := os.Remove(config); err != nil {
				t.Fatal(err)
			}
			if err := create(config); err != nil {
				t.Fatal(err)
			}
			in := newInspector(t)
			done := make(chan error, 1)
			go func() { _, err := in.Inspect(context.Background(), dir); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the config was read")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the inspection hung")
			}
		})
	}
}

func TestASymlinkedRefIsRefused(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	main := filepath.Join(dir, ".git", "refs", "heads", "main")
	if err := os.Rename(main, filepath.Join(dir, ".git", "main-target")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../main-target", main); err != nil {
		t.Fatal(err)
	}
	if !refused(t, newInspector(t), dir) {
		t.Fatal("a symlinked ref was followed")
	}
}
