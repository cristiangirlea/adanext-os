//go:build windows

package artifact

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func injectedDiskFull() error { return windows.ERROR_DISK_FULL }

func TestHandleDiskFullIsClassified(t *testing.T) {
	if err := classify(windows.ERROR_HANDLE_DISK_FULL); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ERROR_HANDLE_DISK_FULL: %v", err)
	}
}
