//go:build windows

package artifact

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOpenRefusesDirectoryJunctions(t *testing.T) {
	for _, name := range []string{"store", "tmp"} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			dir := filepath.Join(t.TempDir(), "artifacts")
			link := dir
			if name == "tmp" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(dir, "tmp")
			}
			output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
			if err != nil {
				t.Fatalf("create junction: %v: %s", err, output)
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeIrregular == 0 {
				t.Fatalf("junction did not exercise ModeIrregular: %v %v", info, err)
			}
			if _, err := Open(dir); err == nil {
				t.Fatal("accepted directory junction")
			}
		})
	}
}

func TestOpenRefusesADirectorySymlinkWhenWindowsPermitsOne(t *testing.T) {
	for _, name := range []string{"store", "tmp"} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			dir := filepath.Join(t.TempDir(), "artifacts")
			link := dir
			if name == "tmp" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(dir, "tmp")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("creating a directory symlink is not permitted: %v", err)
			}
			if _, err := Open(dir); err == nil {
				t.Fatal("accepted directory symlink")
			}
		})
	}
}
