//go:build !windows

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// checkBackupDestination refuses a destination directory that another user
// could rename entries in, by owning it or one of its ancestors, or by being
// able to write to one: SQLite writes the snapshot by path, so such a user
// could swap the staging directory and receive the database. It resolves dir
// once and returns that path, every component of which it found to be a real
// directory, not a link, that only the user or root can change; the backup
// then works only from it, since a link on the given path could be repointed.
// The path as given is checked too.
func checkBackupDestination(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("check backup destination directory: %w", err)
	}
	for _, start := range []string{dir, resolved} {
		for p := filepath.Clean(start); ; p = filepath.Dir(p) {
			stat := os.Stat
			if start == resolved {
				stat = os.Lstat
			}
			info, err := stat(p)
			if err != nil {
				return "", fmt.Errorf("check backup destination directory: %w", err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("backup destination directory %s: %s became a link", dir, p)
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !trustedBackupDirectory(st.Uid, st.Gid, info.Mode(), os.Geteuid(), os.Getegid()) {
				return "", fmt.Errorf("backup destination directory %s: %s can be changed by another user", dir, p)
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return resolved, nil
}

// trustedBackupDirectory reports whether only the user and root can rename
// entries in a directory with this owner, group and mode: it is owned by one
// of them and is sticky, or is not writable by others and group-writable only
// for the user's primary group. In a sticky directory, such as /tmp, only an
// entry's owner, the directory's owner and root can rename it.
func trustedBackupDirectory(uid, gid uint32, mode os.FileMode, euid, egid int) bool {
	if uid != uint32(euid) && uid != 0 {
		return false
	}
	if mode&os.ModeSticky != 0 {
		return true
	}
	return mode&0o002 == 0 && (mode&0o020 == 0 || gid == uint32(egid))
}

// openBackupLock opens or creates the lock file itself, never a symlink's target.
func openBackupLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	lock := os.NewFile(uintptr(fd), path)
	info, err := lock.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("backup lock %s is not a regular file", path)
	}
	if err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// tryLockBackup takes an flock, which belongs to the open file and so also
// excludes another Store of this process, without waiting for it.
func tryLockBackup(lock *os.File) (bool, error) {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func unlockBackup(lock *os.File) error {
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}

// stagedFile is what the sweep saw of a staging entry, without following it.
type stagedFile struct{ info os.FileInfo }

func inspectStaged(path string) (stagedFile, error) {
	info, err := os.Lstat(path)
	return stagedFile{info}, err
}

func (f stagedFile) regular() bool      { return f.info.Mode().IsRegular() }
func (f stagedFile) modTime() time.Time { return f.info.ModTime() }

// removeStaged unlinks path only while it is still the regular file seen. An
// unlink never follows a symlink and never removes a directory, so whatever
// replaces the file in the last instant is at worst unlinked itself.
func removeStaged(path string, seen stagedFile) error {
	now, err := inspectStaged(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !now.regular() || !os.SameFile(seen.info, now.info) || !now.modTime().Equal(seen.modTime()) {
		return nil
	}
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
