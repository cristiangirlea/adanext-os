//go:build windows

package source

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openShared opens a user's file for reading with FILE_SHARE_DELETE, so the
// user's git can rename over it while Ada holds it. It opens a final reparse
// point itself instead of its target, and refuses it, a remote path and any
// handle that is not a regular file on disk.
func openShared(path string) (*os.File, error) {
	if IsRemotePath(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errRemote}
	}
	name := path
	if len(name) >= 248 && filepath.VolumeName(name) != "" && !strings.HasPrefix(name, `\\`) {
		name = `\\?\` + filepath.Clean(name)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
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
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errLink}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errNotRegular}
	}
	return os.NewFile(uintptr(h), path), nil
}
