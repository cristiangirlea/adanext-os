package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertNoBackupTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".adanext-backup-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("backup temporary files remain: %v", matches)
	}
}

func vacuumIntoBackupTemp(t *testing.T, s *Store, dir string) string {
	t.Helper()
	file, err := os.CreateTemp(dir, ".adanext-backup-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", file.Name()); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestBackupRefusesAnExistingDestinationWithoutChangingIt(t *testing.T) {
	for _, contents := range [][]byte{nil, []byte("existing backup contents")} {
		name := "empty"
		if len(contents) != 0 {
			name = "nonempty"
		}
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			dest := filepath.Join(t.TempDir(), "backup.sqlite")
			if err := os.WriteFile(dest, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.Backup(ctx, dest); !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "backup destination exists") {
				t.Fatalf("backup of an existing destination: got %v, want os.ErrExist", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("existing destination was removed: %v", err)
			}
			if !bytes.Equal(got, contents) {
				t.Fatalf("existing destination changed: got %q, want %q", got, contents)
			}
			assertNoBackupTemps(t, filepath.Dir(dest))
		})
	}
}

func TestFailedVacuumLeavesNeitherTemporaryFileNorDestination(t *testing.T) {
	s := openTestStore(t)
	dest := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("backup of a closed store succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed backup destination remains: %v", err)
	}
	assertNoBackupTemps(t, filepath.Dir(dest))
}

func TestBackupRemovesTemporaryFileAfterSyncFailureAndCanRetry(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.sqlite")
	tmp := vacuumIntoBackupTemp(t, s, dir)
	failure := errors.New("injected sync failure")
	err := finishBackup(tmp, dest, func(string) (backupFile, error) {
		return &failingBackupFile{syncErr: failure}, nil
	}, publishBackupNoReplace, syncBackupDirectory)
	if !errors.Is(err, failure) {
		t.Fatalf("sync failure: %v", err)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after sync failure: %v", err)
	}
	assertNoBackupTemps(t, dir)
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("retry backup: %v", err)
	}
	assertNoBackupTemps(t, dir)
}

func TestBackupDoesNotReplaceDestinationCreatedBeforePublish(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.sqlite")
	tmp := vacuumIntoBackupTemp(t, s, dir)
	contents := []byte("someone else's file")
	err := finishBackup(tmp, dest, func(path string) (backupFile, error) {
		return os.OpenFile(path, os.O_RDWR, 0)
	}, func(source, target string) error {
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			return err
		}
		return publishBackupNoReplace(source, target)
	}, syncBackupDirectory)
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "backup destination exists") {
		t.Fatalf("publish over existing destination: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("other destination changed: %q, %v", got, err)
	}
	assertNoBackupTemps(t, dir)
}

type failingBackupFile struct {
	syncErr, closeErr error
	closed            bool
}

func (f *failingBackupFile) Sync() error  { return f.syncErr }
func (f *failingBackupFile) Close() error { f.closed = true; return f.closeErr }

func TestAnUnsyncedBackupIsRemovedAndItsCauseIsPreserved(t *testing.T) {
	failure := errors.New("injected disk failure")
	for _, stage := range []string{"open", "file sync", "close", "publish", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			dest := filepath.Join(t.TempDir(), "backup.sqlite")
			// Create a real SQLite snapshot, then inject only the filesystem fault.
			tmp := vacuumIntoBackupTemp(t, s, filepath.Dir(dest))
			file := &failingBackupFile{}
			if stage == "file sync" {
				file.syncErr = failure
			}
			if stage == "close" {
				file.closeErr = failure
			}
			publish := publishBackupNoReplace
			if stage == "publish" {
				publish = func(string, string) error { return failure }
			}
			err := finishBackup(tmp, dest, func(string) (backupFile, error) {
				if stage == "open" {
					return nil, failure
				}
				return file, nil
			}, publish, func(string) error {
				if stage == "directory sync" {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("cause: %v", err)
			}
			if stage == "publish" && !strings.Contains(err.Error(), "publish backup") {
				t.Fatalf("publish failure context: %v", err)
			}
			if stage != "open" && !file.closed {
				t.Fatal("backup handle was not closed")
			}
			if stage == "directory sync" {
				if _, err := os.Stat(dest); err != nil {
					t.Fatalf("published backup missing after directory sync failure: %v", err)
				}
				if err := os.Remove(dest); err != nil {
					t.Fatal(err)
				}
			} else if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed backup remains: %v", err)
			}
			assertNoBackupTemps(t, filepath.Dir(dest))
			// The failed destination can be used for the next successful backup.
			if err := s.Backup(ctx, dest); err != nil {
				t.Fatal(err)
			}
			assertNoBackupTemps(t, filepath.Dir(dest))
		})
	}
}
