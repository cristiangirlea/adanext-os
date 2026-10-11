//go:build unix

package engine

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading without following a symbolic link in its last element
// (O_NOFOLLOW fails with ELOOP) and without blocking on a FIFO or device (O_NONBLOCK; reads of a
// regular file are unaffected).
func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
