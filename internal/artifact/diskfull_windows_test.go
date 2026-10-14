//go:build windows

package artifact

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func injectedDiskFull() error { return windows.ERROR_DISK_FULL }

func TestHandleDiskFullIsClassified(t *testing.T) {
	if err := classify(windows.ERROR_HANDLE_DISK_FULL); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ERROR_HANDLE_DISK_FULL: %v", err)
	}
}

func TestQuotaExceededIsClassifiedAsDiskFull(t *testing.T) {
	s := testStore(t)
	s.fs = failingFS{fail: "rename", fault: syscall.Errno(1295)}
	_, err := s.Write(context.Background(), strings.NewReader("quota"), 100)
	if !errors.Is(err, ErrDiskFull) || !errors.Is(err, syscall.Errno(1295)) {
		t.Fatalf("quota exceeded: %v", err)
	}
}
