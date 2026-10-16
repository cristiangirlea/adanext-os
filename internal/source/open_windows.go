//go:build windows

package source

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openShared opens a user's file for reading with FILE_SHARE_DELETE, so the
// user's git can rename over it while Ada holds it. It opens a final reparse
// point itself instead of its target, and refuses it, a remote path and any
// handle that is not a regular file on disk; a cloud or dedup reparse point
// is read as the file it is.
func openShared(path string) (*os.File, error) {
	if IsRemotePath(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errRemote}
	}
	h, err := openReparsePoint(path, windows.GENERIC_READ)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			err = os.ErrNotExist
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if kind, err := windows.GetFileType(h); err != nil || kind != windows.FILE_TYPE_DISK {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errNotRegular}
	}
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if tag, err := reparseTag(h); err != nil || isNameSurrogate(tag) {
			windows.CloseHandle(h)
			return nil, &os.PathError{Op: "open", Path: path, Err: errLink}
		}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errNotRegular}
	}
	return os.NewFile(uintptr(h), path), nil
}

// isNameSurrogate is a reparse tag that redirects to another name (a symlink
// or junction), unlike a cloud or dedup file's.
func isNameSurrogate(tag uint32) bool { return tag&0x20000000 != 0 }

// openReparsePoint opens path itself, never a final reparse point's target;
// BACKUP_SEMANTICS lets it open a directory or junction so it can be refused.
func openReparsePoint(path string, access uint32) (windows.Handle, error) {
	name := path
	if len(name) >= 248 && filepath.VolumeName(name) != "" && !strings.HasPrefix(name, `\\`) {
		name = `\\?\` + filepath.Clean(name)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// reparseTag is the handle's reparse tag, 0 when it is not a reparse point.
func reparseTag(h windows.Handle) (uint32, error) {
	var tag struct{ FileAttributes, ReparseTag uint32 } // FILE_ATTRIBUTE_TAG_INFO
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return 0, err
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, nil
	}
	return tag.ReparseTag, nil
}

// isLink is a symlink, or a reparse point whose tag redirects to another name
// or cannot be read; a cloud or dedup file is not one.
func isLink(path string, info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if info.Mode()&os.ModeIrregular == 0 {
		return false
	}
	h, err := openReparsePoint(path, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	tag, err := reparseTag(h)
	return err != nil || isNameSurrogate(tag)
}
