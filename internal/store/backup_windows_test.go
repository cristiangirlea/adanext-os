//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestExtendedPathPrefixesOnlyLongPaths(t *testing.T) {
	long := `C:\` + strings.Repeat(`directory\`, 30) + "backup.sqlite"
	longUNC := `\\server\share\` + strings.Repeat(`directory\`, 30) + "backup.sqlite"
	for _, tc := range []struct{ in, want string }{
		{`C:\short\backup.sqlite`, `C:\short\backup.sqlite`},
		{long, `\\?\` + long},
		{longUNC, `\\?\UNC` + longUNC[1:]},
		{`\\?\` + long, `\\?\` + long},
		{strings.ReplaceAll(long, `\`, `/`), `\\?\` + long},
	} {
		if got := extendedPath(tc.in); got != tc.want {
			t.Errorf("extendedPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBackupPublishesToAPathLongerThanMaxPath(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 60))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "backup.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("backup to a %d-character path: %v", len(dest), err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	assertNoBackupTemps(t, dir)
}

func TestWindowsPublishWritesThroughAndNeverReplaces(t *testing.T) {
	var flags uint32
	called := false
	original := moveFileEx
	moveFileEx = func(from, to *uint16, f uint32) error {
		called = true
		flags = f
		return nil
	}
	defer func() { moveFileEx = original }()
	if err := publishBackupNoReplace(`C:\backups\.adanext-backup-1.tmp`, `C:\backups\backup.sqlite`); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("MoveFileEx was not called")
	}
	if flags&windows.MOVEFILE_WRITE_THROUGH == 0 {
		t.Errorf("publish flags %#x lack MOVEFILE_WRITE_THROUGH", flags)
	}
	if flags&windows.MOVEFILE_REPLACE_EXISTING != 0 {
		t.Errorf("publish flags %#x allow replacing the destination", flags)
	}
}
