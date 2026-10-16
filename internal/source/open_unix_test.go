//go:build !windows

package source

import (
	"context"
	"errors"
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

func TestReadFileRefusesAFinalSymlink(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	write(t, filepath.Join(dir, "file"), "x\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFile(link, maxFileSize); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("readFile through a symlink = %v", err)
	}
}

func TestARemoteTargetBehindALinkInAPointerIsRefused(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T) string{
		"an alternate": func(t *testing.T) string {
			dir, links := newRepo(t, nil), tempDir(t)
			if err := os.Symlink("//host/share", filepath.Join(links, "l")); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), filepath.Join(links, "l", "objects")+"\n")
			return dir
		},
		"gitdir": func(t *testing.T) string {
			dir := tempDir(t)
			if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("//host/share", filepath.Join(dir, "sub", "l")); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, ".git"), "gitdir: sub/l/x.git\n")
			return dir
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := inspect(t, newInspector(t), setup(t))
			if _, ok := blocker(r, BlockRemotePath); !ok {
				t.Fatalf("blockers = %+v", r.Blockers)
			}
		})
	}
}

func TestALinkWhoseDotDotFollowsAnIgnoredLinkOutsideIsABlocker(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{".gitignore": "hidden\n"})
	outside := tempDir(t)
	write(t, filepath.Join(outside, "a", "f"), "a\n")
	write(t, filepath.Join(outside, "x"), "outside\n")
	if err := os.Symlink(filepath.Join(outside, "a"), filepath.Join(dir, "hidden")); err != nil {
		t.Fatal(err)
	}
	// hidden/../x is outside/x, not the work tree's x.
	if err := os.Symlink("hidden/../x", filepath.Join(dir, "visible")); err != nil {
		t.Fatal(err)
	}
	r := inspect(t, newInspector(t), dir)
	if b, ok := blocker(r, BlockEscapingLink); !ok || !hasPath(b.Paths, "visible") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}
