//go:build !windows

package artifact

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
)

func injectedDiskFull() error { return syscall.ENOSPC }

func TestQuotaExceededIsClassifiedAsDiskFull(t *testing.T) {
	s := testStore(t)
	s.fs = failingFS{fail: "rename", fault: syscall.EDQUOT}
	_, err := s.Write(context.Background(), strings.NewReader("quota"), 100)
	if !errors.Is(err, ErrDiskFull) || !errors.Is(err, syscall.EDQUOT) {
		t.Fatalf("quota exceeded: %v", err)
	}
}
