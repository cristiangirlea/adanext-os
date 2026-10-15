package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var (
	// ErrPublishedWithWarning says a backup is complete at its destination
	// although a step after publishing it failed; the cause is wrapped. Do not
	// retry the backup to the same path: that would report an existing
	// destination.
	ErrPublishedWithWarning = errors.New("backup published, but a step after publishing failed")
	// ErrPublishUnsupported says the destination filesystem has neither hard
	// links nor a no-replace rename, so a backup cannot be published there
	// without the risk of replacing a file.
	ErrPublishUnsupported = errors.New("backup destination filesystem supports neither hard links nor a no-replace rename")
)

// backupTempPattern names the temporary files a backup is written to before
// it is published. It must stay a filepath.Match pattern.
const backupTempPattern = ".adanext-backup-*.tmp"

// staleBackupTempAge is how old a temporary backup must be before a later
// backup treats it as left behind by a crash.
const staleBackupTempAge = time.Hour

// publishNoReplace publishes tmpPath at destPath without replacing an existing
// file: by hard link, falling back to a no-replace rename where the filesystem
// has no hard links (exFAT and FAT drives, some SMB and FUSE mounts). It
// refuses with ErrPublishUnsupported where neither exists.
func publishNoReplace(link, rename func(tmpPath, destPath string) error, tmpPath, destPath string) error {
	linkErr := link(tmpPath, destPath)
	if linkErr == nil || !hardLinksUnsupported(linkErr) {
		return linkErr
	}
	renameErr := rename(tmpPath, destPath)
	if renameErr == nil {
		return nil
	}
	if errors.Is(renameErr, ErrPublishUnsupported) || noReplaceRenameUnsupported(renameErr) {
		return fmt.Errorf("%w: link: %v; rename: %v", ErrPublishUnsupported, linkErr, renameErr)
	}
	return fmt.Errorf("rename after link failed (%v): %w", linkErr, renameErr)
}

// hardLinksUnsupported reports a link failure that says the filesystem has no
// hard links, as opposed to one about the paths involved.
func hardLinksUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// noReplaceRenameUnsupported reports a rename failure that says the kernel or
// filesystem has no no-replace rename (EINVAL for an unsupported flag, ENOSYS
// before Linux 3.15, ENOTSUP on macOS).
func noReplaceRenameUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

// sweepStaleBackupTemps removes the temporary backups in dir that a crash left
// behind, each a full copy of the database: regular files matching
// backupTempPattern, older than staleBackupTempAge, and never keep, the one
// being written. It is best effort: a leftover it cannot remove waits for the
// next backup.
func sweepStaleBackupTemps(dir, keep string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if match, _ := filepath.Match(backupTempPattern, name); !match || name == filepath.Base(keep) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < staleBackupTempAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
