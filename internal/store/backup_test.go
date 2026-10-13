package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
			if err := s.Backup(ctx, dest); !errors.Is(err, os.ErrExist) {
				t.Fatalf("backup of an existing destination: got %v, want os.ErrExist", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("existing destination was removed: %v", err)
			}
			if !bytes.Equal(got, contents) {
				t.Fatalf("existing destination changed: got %q, want %q", got, contents)
			}
		})
	}
}

func TestBackupRemovesItsDestinationWhenVacuumFails(t *testing.T) {
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
}

type failingBackupFile struct {
	syncErr, closeErr error
	closed            bool
}

func (f *failingBackupFile) Sync() error  { return f.syncErr }
func (f *failingBackupFile) Close() error { f.closed = true; return f.closeErr }

func TestAnUnsyncedBackupIsRemovedAndItsCauseIsPreserved(t *testing.T) {
	failure := errors.New("injected disk failure")
	for _, stage := range []string{"open", "file sync", "close", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			dest := filepath.Join(t.TempDir(), "backup.sqlite")
			// Create a real SQLite snapshot, then inject only the filesystem fault.
			if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
				t.Fatal(err)
			}
			file := &failingBackupFile{}
			if stage == "file sync" {
				file.syncErr = failure
			}
			if stage == "close" {
				file.closeErr = failure
			}
			err := finishBackup(dest, func(string) (backupFile, error) {
				if stage == "open" {
					return nil, failure
				}
				return file, nil
			}, func(string) error {
				if stage == "directory sync" {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("cause: %v", err)
			}
			if stage != "open" && !file.closed {
				t.Fatal("backup handle was not closed")
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed backup remains: %v", err)
			}
			// The failed destination can be used for the next successful backup.
			if err := s.Backup(ctx, dest); err != nil {
				t.Fatal(err)
			}
		})
	}
}
