package store

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openStaged opens path itself, never what a reparse point there names;
// backup semantics let a directory or a junction open, so its attributes, not
// an access error, are what tell it apart.
func openStaged(path string, access, disposition uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		disposition, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// stagedFile is what the sweep saw of a staging entry through its handle,
// with the identity a later handle must match.
type stagedFile struct {
	attributes                  uint32
	written                     time.Time
	volume, indexHigh, indexLow uint32
}

func stagedInfo(h windows.Handle, path string) (stagedFile, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return stagedFile{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return stagedFile{
		attributes: info.FileAttributes,
		written:    time.Unix(0, info.LastWriteTime.Nanoseconds()),
		volume:     info.VolumeSerialNumber,
		indexHigh:  info.FileIndexHigh,
		indexLow:   info.FileIndexLow,
	}, nil
}

func (f stagedFile) regular() bool {
	return f.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) == 0
}

func (f stagedFile) modTime() time.Time { return f.written }

func inspectStaged(path string) (stagedFile, error) {
	h, err := openStaged(path, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		return stagedFile{}, err
	}
	defer windows.CloseHandle(h)
	return stagedInfo(h, path)
}

// removeStaged deletes the file at path through the handle it checks, so only
// the regular file seen is deleted, never a reparse point or a replacement.
func removeStaged(path string, seen stagedFile) error {
	h, err := openStaged(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	now, err := stagedInfo(h, path)
	if err != nil {
		return err
	}
	if !now.regular() || now.volume != seen.volume || now.indexHigh != seen.indexHigh || now.indexLow != seen.indexLow || !now.written.Equal(seen.written) {
		return nil
	}
	disposition := struct{ DeleteFile bool }{true}
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition))); err != nil {
		return &os.PathError{Op: "delete", Path: path, Err: err}
	}
	return nil
}

// checkBackupDestination accepts any directory on Windows and returns it
// unchanged: the held lock handle pins the real staging directory and its
// ancestors against renames, but not a junction or symlink on the given path.
func checkBackupDestination(dir string) (string, error) { return dir, nil }

// openBackupLock opens or creates the lock file itself, never a reparse
// point's target.
func openBackupLock(path string) (*os.File, error) {
	h, err := openStaged(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
	if err != nil {
		return nil, err
	}
	info, err := stagedInfo(h, path)
	if err == nil && !info.regular() {
		err = fmt.Errorf("backup lock %s is not a regular file", path)
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// tryLockBackup takes a LockFileEx lock, which belongs to the handle and so
// also excludes another Store of this process, without waiting for it.
func tryLockBackup(lock *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// unlockBackup releases the lock before the handle closes: Windows frees a
// closed handle's locks only when it gets round to it.
func unlockBackup(lock *os.File) error {
	return windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, &windows.Overlapped{})
}
