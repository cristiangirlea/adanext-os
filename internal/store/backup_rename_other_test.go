//go:build !windows && !linux && !darwin

package store

import (
	"errors"
	"syscall"
	"testing"
)

func TestBackupRefusesToPublishWhereHardLinksAndNoReplaceRenameAreMissing(t *testing.T) {
	if err := renameNoReplace("from", "to"); !errors.Is(err, ErrPublishUnsupported) {
		t.Fatalf("renameNoReplace: %v, want ErrPublishUnsupported", err)
	}
	noHardLinks := func(string, string) error { return syscall.EPERM }
	if err := publishNoReplace(noHardLinks, renameNoReplace, "from", "to"); !errors.Is(err, ErrPublishUnsupported) {
		t.Fatalf("publish without hard links: %v, want ErrPublishUnsupported", err)
	}
}
