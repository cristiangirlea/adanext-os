//go:build linux || darwin

package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBackupPublishesByNoReplaceRenameWhereHardLinksAreMissing(t *testing.T) {
	noHardLinks := func(string, string) error { return syscall.EPERM }
	fs := osBackupFS()
	fs.publish = func(tmpPath, destPath string) error {
		return publishNoReplace(noHardLinks, renameNoReplace, tmpPath, destPath)
	}

	t.Run("fresh destination", func(t *testing.T) {
		s := openTestStore(t)
		seed(t, s)
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.sqlite")
		tmp := vacuumIntoBackupTemp(t, s, dir)
		if err := finishBackup(tmp, dest, fs); err != nil {
			t.Fatalf("publish by rename: %v", err)
		}
		restored, err := Open(ctx, dest)
		if err != nil {
			t.Fatalf("renamed backup is not a usable database: %v", err)
		}
		defer restored.Close()
		assertNoBackupTemps(t, dir)
	})

	t.Run("existing destination", func(t *testing.T) {
		s := openTestStore(t)
		seed(t, s)
		dir := t.TempDir()
		dest := filepath.Join(dir, "backup.sqlite")
		contents := []byte("existing backup contents")
		if err := os.WriteFile(dest, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		tmp := vacuumIntoBackupTemp(t, s, dir)
		if err := finishBackup(tmp, dest, fs); !errors.Is(err, os.ErrExist) {
			t.Fatalf("rename over an existing destination: %v, want os.ErrExist", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(got, contents) {
			t.Fatalf("existing destination changed: %q, %v", got, err)
		}
		assertNoBackupTemps(t, dir)
	})
}
