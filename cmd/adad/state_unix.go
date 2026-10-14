//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

func validateStateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("must be a directory and not group- or other-writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("must be owned by the current user")
	}
	return nil
}
