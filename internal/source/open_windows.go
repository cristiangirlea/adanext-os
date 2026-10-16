//go:build windows

package source

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openShared opens a user's file for reading with FILE_SHARE_DELETE, so the
// user's git can rename over it while Ada holds it.
func openShared(path string) (*os.File, error) {
	name := path
	if len(name) >= 248 && filepath.VolumeName(name) != "" && !strings.HasPrefix(name, `\\`) {
		name = `\\?\` + filepath.Clean(name)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			err = os.ErrNotExist
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
