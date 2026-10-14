//go:build windows

package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

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
