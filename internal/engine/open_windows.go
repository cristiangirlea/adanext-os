//go:build windows

package engine

import (
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path for reading without following a symbolic link or other reparse point
// (FILE_FLAG_OPEN_REPARSE_POINT opens the point itself), and refuses what it opened if that is a
// reparse point or a directory. Windows has no FIFOs in the file namespace, so nothing here can
// block on one.
func openNoFollow(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		_ = syscall.CloseHandle(h)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&(syscall.FILE_ATTRIBUTE_REPARSE_POINT|syscall.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = syscall.CloseHandle(h)
		return nil, fmt.Errorf("%s is a reparse point or a directory, not a regular file", path)
	}
	return os.NewFile(uintptr(h), path), nil
}
