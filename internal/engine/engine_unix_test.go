//go:build unix

package engine

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestIdentifyRefusesAFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "hrn")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Identify(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Identify accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Identify blocked opening a FIFO with no writer")
	}
}
