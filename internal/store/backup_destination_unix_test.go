//go:build !windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRefusesADestinationDirectoryOthersCanRenameIn(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	cases := []struct {
		name string
		mode os.FileMode
		ok   bool
	}{
		{"writable by others", 0o777, false},
		{"writable by others and sticky", 0o777 | os.ModeSticky, true},
		{"private", 0o700, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shared")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, c.mode); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(dir, "backup.sqlite")
			err := s.Backup(ctx, dest)
			if c.ok {
				if err != nil {
					t.Fatalf("backup into a %v directory: %v", c.mode, err)
				}
				assertSQLiteFile(t, dest)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "another user") {
				t.Fatalf("backup into a %v directory: %v, want a refusal", c.mode, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, backupStagingName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused backup created its staging directory: %v", err)
			}
		})
	}
}

func TestBackupRefusesADestinationBelowADirectoryOthersCanRenameIn(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	shared := filepath.Join(t.TempDir(), "shared")
	dir := filepath.Join(shared, "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, filepath.Join(dir, "backup.sqlite")); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("backup below a 0777 directory: %v, want a refusal", err)
	}
}

func TestTrustedBackupDirectory(t *testing.T) {
	const euid, egid = 1000, 1000
	cases := []struct {
		name     string
		uid, gid uint32
		mode     os.FileMode
		want     bool
	}{
		{"the user's", euid, egid, 0o700, true},
		{"root's", 0, 0, 0o755, true},
		{"another user's", 1001, egid, 0o700, false},
		{"writable by others", euid, egid, 0o777, false},
		{"writable by others and sticky", 0, 0, 0o777 | os.ModeSticky, true},
		{"group-writable, the user's primary group", euid, egid, 0o770, true},
		{"group-writable, another group", euid, 1001, 0o770, false},
		{"group-writable, another group, sticky", 0, 1001, 0o775 | os.ModeSticky, true},
		{"/tmp for a user other than root", 0, 0, 0o777 | os.ModeSticky, true},
		{"another user's, sticky", 1001, 1001, 0o777 | os.ModeSticky, false},
	}
	for _, c := range cases {
		if got := trustedBackupDirectory(c.uid, c.gid, c.mode, euid, egid); got != c.want {
			t.Errorf("%s (uid %d, gid %d, %v): %v, want %v", c.name, c.uid, c.gid, c.mode, got, c.want)
		}
	}
}

func TestBackupWorksOnlyFromTheDestinationsResolvedDirectory(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	realDir := filepath.Join(home, "real")
	other := filepath.Join(root, "other")
	share := filepath.Join(root, "share")
	for _, d := range []string{realDir, other, share} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(share, 0o777); err != nil {
		t.Fatal(err)
	}
	// home/b -> share/current -> home/real, and share is open to everyone.
	current := filepath.Join(share, "current")
	linkDirectory(t, realDir, current)
	b := filepath.Join(home, "b")
	linkDirectory(t, current, b)
	resetBackupSeams(t)
	beforeBackupLock = func(string) {
		if err := os.Remove(current); err != nil {
			t.Fatal(err)
		}
		linkDirectory(t, other, current)
	}
	if err := s.Backup(ctx, filepath.Join(b, "backup.sqlite")); err != nil {
		t.Fatal(err)
	}
	assertSQLiteFile(t, filepath.Join(realDir, "backup.sqlite"))
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatalf("the repointed link's target holds %v, %v", entries, err)
	}
}

func TestBackupRefusesALinkResolvingBelowADirectoryOthersCanRenameIn(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	shared := filepath.Join(t.TempDir(), "shared")
	target := filepath.Join(shared, "private")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	linkDirectory(t, target, link)
	if err := s.Backup(ctx, filepath.Join(link, "backup.sqlite")); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("backup through a link below a 0777 directory: %v, want a refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(target, backupStagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused backup created its staging directory: %v", err)
	}
}
