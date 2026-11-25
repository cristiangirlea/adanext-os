//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// openSource opens path without following a link in its last component; it must be a regular file.
func openSource(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		syscall.Close(fd)
		return nil, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openPrivate is openSource for a file only the effective user owns and can reach.
func openPrivate(path string) (*os.File, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		f.Close()
		return nil, errNotOwnerOnly
	}
	return f, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
