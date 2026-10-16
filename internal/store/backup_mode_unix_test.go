//go:build !windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStoreAndBackupFilesArePrivateWithPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0o022)
	defer syscall.Umask(oldUmask)

	path := filepath.Join(t.TempDir(), "store.sqlite")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE mode_probe (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO mode_probe (value) VALUES ('written')"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	assertNoBackupTemps(t, filepath.Dir(dest))
	for _, file := range []string{path, path + "-wal", path + "-shm", dest} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("stat %s: %v", file, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", file, got)
		}
	}
	staging := filepath.Join(filepath.Dir(dest), backupStagingName)
	for file, want := range map[string]os.FileMode{staging: 0o700, filepath.Join(staging, backupLockName): 0o600} {
		info, err := os.Lstat(file)
		if err != nil {
			t.Fatalf("stat %s: %v", file, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", file, got, want)
		}
	}
}

func TestOpenPreservesExistingStoreFileMode(t *testing.T) {
	oldUmask := syscall.Umask(0o022)
	defer syscall.Umask(oldUmask)

	path := filepath.Join(t.TempDir(), "store.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE mode_probe (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO mode_probe (value) VALUES ('written')"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("store mode = %04o, want 0640", got)
	}
}

func TestBackupRefusesPlantedSymlink(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE mode_probe (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO mode_probe (value) VALUES ('written')"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name           string
		existingTarget bool
	}{
		{name: "missing target"},
		{name: "existing target", existingTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.sqlite")
			link := filepath.Join(dir, "backup.sqlite")
			contents := []byte("existing target contents")
			if tc.existingTarget {
				if err := os.WriteFile(target, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			if err := s.Backup(ctx, link); !errors.Is(err, os.ErrExist) {
				t.Fatalf("Backup to symlink error = %v, want os.ErrExist", err)
			}
			assertNoBackupTemps(t, dir)
			info, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("backup path mode = %s, want symlink", info.Mode())
			}
			gotTarget, err := os.Readlink(link)
			if err != nil {
				t.Fatal(err)
			}
			if gotTarget != target {
				t.Errorf("symlink target = %q, want %q", gotTarget, target)
			}
			if tc.existingTarget {
				gotContents, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if string(gotContents) != string(contents) {
					t.Errorf("target contents = %q, want %q", gotContents, contents)
				}
			} else if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat missing target error = %v, want os.ErrNotExist", err)
			}
		})
	}
}
