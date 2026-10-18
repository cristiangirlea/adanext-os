//go:build !windows

package ipc

import (
	"fmt"
	"os"
	"syscall"
)

func createTokenFile(dir string) (*os.File, error) { return os.CreateTemp(dir, ".adad-token-") }

// createOwnerOnly creates a new 0600 file; O_EXCL refuses any existing name, a symlink included.
func createOwnerOnly(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
