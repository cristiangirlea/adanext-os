package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// age sets a file's times to twice the stale limit in the past.
func age(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * staleBackupAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// stagingEntries lists the names in dir's backup staging directory.
func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, backupStagingName))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func assertSQLiteFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("backup %s: %v", path, err)
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatalf("backup %s is not a database: %.16q", path, data)
	}
}

// resetBackupSeams clears the backup test seams when the test ends.
func resetBackupSeams(t *testing.T) {
	t.Cleanup(func() {
		beforeBackupLock = nil
		afterBackupStaged = nil
		backupLockBusy = nil
		beforeStaleBackupRemoval = nil
	})
}

func TestBackupWritesItsTemporaryInAPrivateStagingDirectoryAndLeavesOnlyTheLock(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	resetBackupSeams(t)
	var staged string
	afterBackupStaged = func(tmp string) { staged = tmp }
	dest := filepath.Join(dir, "backup.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(staged) != filepath.Join(dir, backupStagingName) {
		t.Fatalf("temporary %s is not in the staging directory", staged)
	}
	assertSQLiteFile(t, dest)
	if got := stagingEntries(t, dir); len(got) != 1 || got[0] != backupLockName {
		t.Fatalf("staging directory holds %v, want only the lock", got)
	}
}

func TestACompletedBackupOrASourceWithATemporaryLikeNameSurvivesTheSweep(t *testing.T) {
	dir := t.TempDir()
	source, err := Open(ctx, filepath.Join(dir, ".adanext-backup-source.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	seed(t, source)
	completed := filepath.Join(dir, ".adanext-backup-1.tmp")
	if err := source.Backup(ctx, completed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	age(t, completed)
	age(t, filepath.Join(dir, ".adanext-backup-source.tmp"))
	if err := source.Backup(ctx, filepath.Join(dir, "backup.sqlite")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(completed)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("completed backup with a temporary-like name: %d bytes, %v", len(after), err)
	}
	var users int
	if err := source.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&users); err != nil || users != 1 {
		t.Fatalf("source database after the sweep: %d users, %v", users, err)
	}
}

func TestTheSweepRemovesOnlyAgedRegularTemporariesInTheStagingDirectory(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, backupStagingName)
	plant := func(name string) string {
		path := filepath.Join(staging, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	age(t, plant(".adanext-backup-stale.tmp"))
	plant(".adanext-backup-fresh.tmp")
	age(t, plant("notes.txt"))
	agedDir := filepath.Join(staging, ".adanext-backup-dir.tmp")
	if err := os.Mkdir(agedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, agedDir)
	age(t, filepath.Join(staging, backupLockName))
	outside := filepath.Join(dir, ".adanext-backup-outside.tmp")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, outside)
	if err := s.Backup(ctx, filepath.Join(dir, "second.sqlite")); err != nil {
		t.Fatal(err)
	}
	want := []string{".adanext-backup-dir.tmp", ".adanext-backup-fresh.tmp", backupLockName, "notes.txt"}
	if got := stagingEntries(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("staging directory after the sweep: %v, want %v", got, want)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatalf("a temporary-like file outside the staging directory was swept: %v", err)
	}
}

func TestAnAgedTemporaryOfARunningBackupSurvivesAConcurrentBackup(t *testing.T) {
	first, second := openTestStore(t), openTestStore(t)
	seed(t, first)
	seed(t, second)
	dir := t.TempDir()
	resetBackupSeams(t)
	staged := make(chan string, 1)
	resume := make(chan struct{})
	var calls atomic.Int32
	afterBackupStaged = func(tmp string) {
		if calls.Add(1) == 1 {
			staged <- tmp
			<-resume
		}
	}
	busy := make(chan struct{}, 64)
	backupLockBusy = func(string) {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Backup(ctx, filepath.Join(dir, "first.sqlite")) }()
	tmp := <-staged
	age(t, tmp)
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Backup(ctx, filepath.Join(dir, "second.sqlite")) }()
	// Two refusals mean the second backup has started and is still waiting.
	for range 2 {
		select {
		case <-busy:
		case err := <-secondDone:
			t.Fatalf("the second backup ran while the first held the lock: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("the second backup never waited for the lock")
		}
	}
	if _, err := os.Lstat(tmp); err != nil {
		t.Fatalf("the running backup's aged temporary was swept: %v", err)
	}
	close(resume)
	if err := <-firstDone; err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second backup: %v", err)
	}
	assertSQLiteFile(t, filepath.Join(dir, "first.sqlite"))
	assertSQLiteFile(t, filepath.Join(dir, "second.sqlite"))
	if got := stagingEntries(t, dir); len(got) != 1 || got[0] != backupLockName {
		t.Fatalf("staging directory holds %v, want only the lock", got)
	}
}

// TestHelperHoldsTheBackupStagingLock is the other process of the lock test:
// it holds the staging lock until its standard input closes.
func TestHelperHoldsTheBackupStagingLock(t *testing.T) {
	staging := os.Getenv("ADA_BACKUP_LOCK_HELPER")
	if staging == "" {
		return
	}
	_, unlock, err := lockBackupStaging(context.Background(), staging)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := unlock(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestTheStagingLockSerialisesBackupsAcrossProcesses(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, backupStagingName)
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsTheBackupStagingLock$")
	helper.Env = append(os.Environ(), "ADA_BACKUP_LOCK_HELPER="+staging)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		helper.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	resetBackupSeams(t)
	busy := make(chan struct{}, 64)
	backupLockBusy = func(string) {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	dest := filepath.Join(dir, "second.sqlite")
	done := make(chan error, 1)
	go func() { done <- s.Backup(ctx, dest) }()
	<-busy
	<-busy
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup published while another process held the lock: %v", err)
	}
	if got := stagingEntries(t, dir); len(got) != 1 || got[0] != backupLockName {
		t.Fatalf("backup staged while another process held the lock: %v", got)
	}
	stdin.Close()
	if err := <-done; err != nil {
		t.Fatalf("backup after the lock was released: %v", err)
	}
	assertSQLiteFile(t, dest)
	if err := helper.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
}

func TestABackupWaitingForTheLockStopsWithItsContext(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := lockBackupStaging(ctx, filepath.Join(dir, backupStagingName))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	resetBackupSeams(t)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	backupLockBusy = func(string) { cancel() }
	dest := filepath.Join(dir, "second.sqlite")
	if err := s.Backup(waitCtx, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("backup waiting for a held lock: %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled backup destination: %v", err)
	}
}

func TestBackupRefusesADestinationInsideAStagingDirectory(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	for _, dest := range []string{
		filepath.Join(dir, backupStagingName, "backup.sqlite"),
		filepath.Join(dir, strings.ToUpper(backupStagingName), "backup.sqlite"),
		filepath.Join(fresh, backupStagingName),
		filepath.Join(fresh, backupStagingName, "nested", "backup.sqlite"),
	} {
		if err := s.Backup(ctx, dest); err == nil || !strings.Contains(err.Error(), "staging directory") {
			t.Fatalf("backup to %s: %v, want a refusal", dest, err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused destination %s exists: %v", dest, err)
		}
	}
	if got := stagingEntries(t, dir); len(got) != 1 || got[0] != backupLockName {
		t.Fatalf("staging directory holds %v, want only the lock", got)
	}
	if entries, err := os.ReadDir(fresh); err != nil || len(entries) != 0 {
		t.Fatalf("a refused backup created %v: %v", entries, err)
	}
}

func TestBackupRefusesAStagingDirectoryThatIsALink(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	target := t.TempDir()
	linkDirectory(t, target, filepath.Join(dir, backupStagingName))
	dest := filepath.Join(dir, "backup.sqlite")
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("backup through a linked staging directory succeeded")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after the refusal: %v", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("the link's target was written: %v, %v", entries, err)
	}
}

func TestBackupRefusesAStagingDirectorySwappedForALinkBeforeItsLock(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, backupStagingName)
	_, unlock, err := lockBackupStaging(ctx, staging)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			unlock()
		}
	})
	target := t.TempDir()
	stale := filepath.Join(target, ".adanext-backup-stale.tmp")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, stale)
	resetBackupSeams(t)
	var moveErr error
	backupLockBusy = func(string) {
		if released {
			return
		}
		// Swap the checked directory for a link, then let the backup lock.
		if moveErr = os.Rename(staging, filepath.Join(dir, "moved")); moveErr == nil {
			linkDirectory(t, target, staging)
		}
		released = true
		if err := unlock(); err != nil {
			t.Error(err)
		}
	}
	dest := filepath.Join(dir, "second.sqlite")
	err = s.Backup(ctx, dest)
	if moveErr != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("the staging directory cannot be moved while its lock is open: %v", moveErr)
		}
		t.Fatalf("move the staging directory: %v", moveErr)
	}
	if err == nil {
		t.Fatal("backup through a staging directory swapped for a link succeeded")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after the refusal: %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(stale) {
		t.Fatalf("the link's target was written or swept: %v, %v", entries, err)
	}
}

func TestBackupRefusesALockOpenedThroughALinkSwappedBackBeforeTheCheck(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, backupStagingName)
	moved := filepath.Join(dir, "moved")
	target := t.TempDir()
	_, unlockTarget, err := lockBackupStaging(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			unlockTarget()
		}
	})
	resetBackupSeams(t)
	// The checked directory becomes a link before the lock opens, and comes
	// back while the lock in the link's target is busy.
	beforeBackupLock = func(string) {
		if err := os.Rename(staging, moved); err != nil {
			t.Fatal(err)
		}
		linkDirectory(t, target, staging)
	}
	backupLockBusy = func(string) {
		if released {
			return
		}
		if err := os.Remove(staging); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, staging); err != nil {
			t.Fatal(err)
		}
		released = true
		if err := unlockTarget(); err != nil {
			t.Error(err)
		}
	}
	dest := filepath.Join(dir, "second.sqlite")
	if err := s.Backup(ctx, dest); !errors.Is(err, errBackupLockNotChecked) {
		t.Fatalf("backup holding a lock opened through a link: %v, want %v", err, errBackupLockNotChecked)
	}
	if !released {
		t.Fatal("the lock in the link's target was never found busy")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after the refusal: %v", err)
	}
	if got := stagingEntries(t, dir); len(got) != 1 || got[0] != backupLockName {
		t.Fatalf("staging directory holds %v, want only the lock", got)
	}
}

func TestTheSweepNeverFollowsATemporaryReplacedAfterItsInspection(t *testing.T) {
	cases := []struct {
		name    string
		replace func(t *testing.T, path, file, dir string)
	}{
		{"by a fresh regular file", func(t *testing.T, path, _, _ string) {
			if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"by a link to a directory", func(t *testing.T, path, _, dir string) { linkDirectory(t, dir, path) }},
		{"by a symlink to a file", func(t *testing.T, path, file, _ string) {
			if err := os.Symlink(file, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openTestStore(t)
			seed(t, s)
			dir := t.TempDir()
			if err := s.Backup(ctx, filepath.Join(dir, "first.sqlite")); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			victimDir := filepath.Join(outside, "dir")
			if err := os.Mkdir(victimDir, 0o700); err != nil {
				t.Fatal(err)
			}
			inner := filepath.Join(victimDir, ".adanext-backup-inner.tmp")
			if err := os.WriteFile(inner, []byte("keep me too"), 0o600); err != nil {
				t.Fatal(err)
			}
			age(t, inner)
			stale := filepath.Join(dir, backupStagingName, ".adanext-backup-stale.tmp")
			if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			age(t, stale)
			resetBackupSeams(t)
			replaced := false
			beforeStaleBackupRemoval = func(path string) {
				if path != stale || replaced {
					return
				}
				replaced = true
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				c.replace(t, path, victim, victimDir)
			}
			if err := s.Backup(ctx, filepath.Join(dir, "second.sqlite")); err != nil {
				t.Fatal(err)
			}
			if !replaced {
				t.Fatal("the sweep never reached the stale temporary")
			}
			if _, err := os.Lstat(stale); err != nil {
				t.Fatalf("the replacement was removed: %v", err)
			}
			if data, err := os.ReadFile(victim); err != nil || string(data) != "keep me" {
				t.Fatalf("the file outside: %q, %v", data, err)
			}
			if data, err := os.ReadFile(inner); err != nil || string(data) != "keep me too" {
				t.Fatalf("the directory outside: %q, %v", data, err)
			}
		})
	}
}
