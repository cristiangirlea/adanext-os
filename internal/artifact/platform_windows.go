//go:build windows

package artifact

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("artifact directory %s is not a regular directory", path)
	}
	return nil
}
func makePrivateDirectory(string) error      { return nil }
func openBlob(path string) (*os.File, error) { return os.Open(path) }
func syncDirectory(string) error             { return nil }
func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.Errno(1295))
}
