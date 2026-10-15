//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
