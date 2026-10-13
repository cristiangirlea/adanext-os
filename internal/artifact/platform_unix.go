//go:build !windows

package artifact

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact directory %s is a symlink", path)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	return errors.Join(err, closeErr)
}

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }
