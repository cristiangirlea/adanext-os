//go:build !windows

package ipc

import "os"

func createTokenFile(dir string) (*os.File, error) { return os.CreateTemp(dir, ".adad-token-") }
