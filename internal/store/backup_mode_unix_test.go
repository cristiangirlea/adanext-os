//go:build !windows

package store

import (
	"context"
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
	for _, file := range []string{path, path + "-wal", path + "-shm", dest} {
		info, err := os.Stat(file)
		if os.IsNotExist(err) && (file == path+"-wal" || file == path+"-shm") {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", file, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", file, got)
		}
	}
}
