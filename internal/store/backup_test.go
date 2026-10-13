package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
