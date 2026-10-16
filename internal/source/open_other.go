//go:build !windows

package source

import (
	"os"
	"syscall"
)

// openShared opens a regular file without following a final link; O_NONBLOCK
// keeps a FIFO from blocking the open until it is refused.
func openShared(path string) (*os.File, error) {
	if IsRemotePath(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errRemote}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = &os.PathError{Op: "open", Path: path, Err: errNotRegular}
		}
		return nil, err
	}
	return f, nil
}
