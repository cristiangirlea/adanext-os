package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
)

var (
	// ErrPublishedWithWarning says a backup is complete at its destination
	// although a step after publishing it, or the cleanup around it, failed;
	// the cause is wrapped. Do not retry the backup to the same path: that
	// would report an existing destination.
	ErrPublishedWithWarning = errors.New("backup published, but a step after publishing failed")
	// ErrPublishUnsupported says the destination filesystem has neither hard
	// links nor a no-replace rename, so a backup cannot be published there
	// without the risk of replacing a file.
	ErrPublishUnsupported = errors.New("backup destination filesystem supports neither hard links nor a no-replace rename")
)

const (
	// backupTempPattern names the temporary files a backup is written to
	// before it is published.
	backupTempPattern = ".adanext-backup-*.tmp"
	// backupStagingName is the private directory, next to a destination, that
	// holds the temporaries of backups into that directory and their lock.
	backupStagingName = ".adanext-backup-staging"
	// backupLockName is the staging directory's lock file; a backup holds it
	// from before its sweep until its temporary is gone.
	backupLockName = "lock"
	// staleBackupAge is how old a temporary must be before the sweep removes it.
	staleBackupAge = time.Hour
	// backupLockPoll is how often a backup retries a lock another one holds.
	backupLockPoll = 10 * time.Millisecond
)

// Test seams: after a backup has created its temporary, each time a backup
// finds the staging lock held, and between the sweep's inspection of a stale
// temporary and its removal.
var (
	afterBackupStaged        func(tmpPath string)
	backupLockBusy           func(staging string)
	beforeStaleBackupRemoval func(path string)
)

// insideBackupStaging reports whether path is a staging directory or lies
// inside one. Names compare without case, as they do on Windows and macOS.
func insideBackupStaging(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if strings.EqualFold(filepath.Base(p), backupStagingName) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// prepareBackupStaging creates the staging directory in dir, or takes the one
// there, and makes it private. A link in its place is refused, and a missing
// dir is not created.
func prepareBackupStaging(dir string) (string, error) {
	staging := filepath.Join(dir, backupStagingName)
	if err := os.Mkdir(staging, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := artifact.PrivateDirectory(staging); err != nil {
		return "", err
	}
	return staging, nil
}

// lockBackupStaging takes the staging directory's lock, waiting while another
// backup holds it, in this process or another, until ctx ends.
func lockBackupStaging(ctx context.Context, staging string) (unlock func() error, err error) {
	lock, err := openBackupLock(filepath.Join(staging, backupLockName))
	if err != nil {
		return nil, fmt.Errorf("open backup lock: %w", err)
	}
	for {
		locked, err := tryLockBackup(lock)
		if err != nil {
			lock.Close()
			return nil, fmt.Errorf("lock backup staging directory: %w", err)
		}
		if locked {
			return func() error { return errors.Join(unlockBackup(lock), lock.Close()) }, nil
		}
		if backupLockBusy != nil {
			backupLockBusy(staging)
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(backupLockPoll):
		}
	}
}

// sweepStaleBackupTemps removes the temporaries in staging older than
// staleBackupAge, which a crashed backup left behind. The caller holds the
// staging lock, so none belongs to a running backup. An entry is removed only
// while it is still the regular file inspected; anything else stays.
func sweepStaleBackupTemps(staging string) error {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("list backup staging directory: %w", err)
	}
	cutoff := time.Now().Add(-staleBackupAge)
	var failed error
	for _, entry := range entries {
		if matched, _ := filepath.Match(backupTempPattern, entry.Name()); !matched {
			continue
		}
		path := filepath.Join(staging, entry.Name())
		seen, err := inspectStaged(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("inspect stale backup temporary: %w", err))
			continue
		}
		if !seen.regular() || !seen.modTime().Before(cutoff) {
			continue
		}
		if beforeStaleBackupRemoval != nil {
			beforeStaleBackupRemoval(path)
		}
		if err := removeStaged(path, seen); err != nil {
			failed = errors.Join(failed, fmt.Errorf("remove stale backup temporary: %w", err))
		}
	}
	return failed
}

// withCleanup adds a failure of the cleanup around a backup to its result: a
// warning once the backup is published, part of the error before.
func withCleanup(err, cleanup error, step string) error {
	if cleanup == nil {
		return err
	}
	cleanup = fmt.Errorf("%s: %w", step, cleanup)
	if err == nil {
		return fmt.Errorf("%w: %w", ErrPublishedWithWarning, cleanup)
	}
	return errors.Join(err, cleanup)
}

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
