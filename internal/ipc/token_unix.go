//go:build !windows

package ipc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func createTokenFile(dir string) (*os.File, error) { return os.CreateTemp(dir, ".adad-token-") }

// createOwnerOnly creates a new 0600 file; O_EXCL refuses any existing name, a symlink included.
func createOwnerOnly(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func createStartupFile(path string) (*os.File, error) { return createOwnerOnly(path) }

// readToken opens the token without following a link or waiting on a FIFO and checks the
// file it opened: an account that swapped the directory cannot make a 0600 file the user owns.
func readToken(dir string) ([]byte, error) {
	path := filepath.Join(dir, "adad.token")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("ipc: token %q must be a regular file owned by the user with mode 0600", path)
	}
	return io.ReadAll(io.LimitReader(f, 64))
}

// checkTokenDir requires the token's directory to be the user's own and 0700:
// an account that could replace the token could replace the socket too.
func checkTokenDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("ipc: token directory %q must be a real directory owned by the user with mode 0700", dir)
	}
	return nil
}
