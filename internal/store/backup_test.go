package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func assertNoBackupTemps(t *testing.T, dir string) {
	t.Helper()
	for _, where := range []string{dir, filepath.Join(dir, backupStagingName)} {
		matches, err := filepath.Glob(filepath.Join(where, ".adanext-backup-*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("backup temporary files remain: %v", matches)
		}
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
	fs := osBackupFS()
	fs.open = func(string) (backupFile, error) { return &failingBackupFile{syncErr: failure}, nil }
	err := finishBackup(tmp, dest, fs)
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
	fs := osBackupFS()
	fs.publish = func(source, target string) error {
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			return err
		}
		return publishBackupNoReplace(source, target)
	}
	err := finishBackup(tmp, dest, fs)
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
			fs := osBackupFS()
			if stage == "publish" {
				fs.publish = func(string, string) error { return failure }
			}
			fs.open = func(string) (backupFile, error) {
				if stage == "open" {
					return nil, failure
				}
				return file, nil
			}
			fs.syncDir = func(string) error {
				if stage == "directory sync" {
					return failure
				}
				return nil
			}
			err := finishBackup(tmp, dest, fs)
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

func TestPublishFallsBackToANoReplaceRenameWhereHardLinksAreMissing(t *testing.T) {
	for _, linkErr := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		t.Run(linkErr.Error(), func(t *testing.T) {
			var renamed [][2]string
			err := publishNoReplace(func(string, string) error { return linkErr }, func(from, to string) error {
				renamed = append(renamed, [2]string{from, to})
				return nil
			}, "tmp", "dest")
			if err != nil {
				t.Fatalf("publish without hard links: %v", err)
			}
			if len(renamed) != 1 || renamed[0] != [2]string{"tmp", "dest"} {
				t.Fatalf("renames: %v, want tmp to dest once", renamed)
			}
		})
	}
}

func TestPublishDoesNotRenameAfterOtherLinkFailures(t *testing.T) {
	for _, linkErr := range []error{syscall.EEXIST, syscall.EIO} {
		t.Run(linkErr.Error(), func(t *testing.T) {
			err := publishNoReplace(func(string, string) error { return linkErr }, func(string, string) error {
				t.Fatal("renamed after a link failure that does not say hard links are missing")
				return nil
			}, "tmp", "dest")
			if !errors.Is(err, linkErr) {
				t.Fatalf("link failure: %v, want %v", err, linkErr)
			}
		})
	}
}

func TestPublishRefusesPlainlyWhereNeitherHardLinksNorNoReplaceRenameExist(t *testing.T) {
	link := func(string, string) error { return syscall.EPERM }
	for _, tc := range []struct {
		name      string
		renameErr error
	}{
		{"old kernel", syscall.ENOSYS},
		{"filesystem without the flag", syscall.EINVAL},
		{"macOS filesystem without the flag", syscall.ENOTSUP},
		{"no rename at all", ErrPublishUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := publishNoReplace(link, func(string, string) error { return tc.renameErr }, "tmp", "dest")
			if !errors.Is(err, ErrPublishUnsupported) {
				t.Fatalf("publish: %v, want ErrPublishUnsupported", err)
			}
			if !strings.Contains(err.Error(), syscall.EPERM.Error()) {
				t.Fatalf("publish error hides the link failure: %v", err)
			}
		})
	}
	t.Run("rename fails for another reason", func(t *testing.T) {
		err := publishNoReplace(link, func(string, string) error { return syscall.EEXIST }, "tmp", "dest")
		if !errors.Is(err, syscall.EEXIST) || errors.Is(err, ErrPublishUnsupported) {
			t.Fatalf("rename failure: %v, want EEXIST without ErrPublishUnsupported", err)
		}
	})
}

func TestAFailureAfterPublishingReportsACompleteBackup(t *testing.T) {
	failure := errors.New("injected failure after publish")
	for _, stage := range []string{"remove temporary", "directory sync"} {
		t.Run(stage, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			dir := t.TempDir()
			dest := filepath.Join(dir, "backup.sqlite")
			tmp := vacuumIntoBackupTemp(t, s, dir)
			fs := osBackupFS()
			if stage == "remove temporary" {
				fs.remove = func(string) error { return failure }
			} else {
				fs.syncDir = func(string) error { return failure }
			}
			err := finishBackup(tmp, dest, fs)
			if !errors.Is(err, ErrPublishedWithWarning) {
				t.Fatalf("failure after publish: %v, want ErrPublishedWithWarning", err)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("cause lost: %v", err)
			}
			restored, err := Open(ctx, dest)
			if err != nil {
				t.Fatalf("published backup is not a usable database: %v", err)
			}
			defer restored.Close()
			var tasks int
			if err := restored.db.QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 1 {
				t.Fatalf("backup tasks: %d, %v", tasks, err)
			}
		})
	}
}
