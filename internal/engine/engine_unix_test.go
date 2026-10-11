//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// identifyWithin runs Identify and fails the test if it does not return in time, so a blocking
// open shows as a failure instead of a hung test run.
func identifyWithin(t *testing.T, path string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := Identify(path); done <- err }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Identify blocked")
		return nil
	}
}

func TestIdentifyRefusesAFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "hrn")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	if err := identifyWithin(t, fifo); err == nil {
		t.Fatal("Identify accepted a FIFO")
	}
}

// swapBeforeOpen replaces the checked path between Identify's check and its open, the window a
// concurrent replacement would use.
func swapBeforeOpen(t *testing.T, swap func(path string)) {
	t.Helper()
	old := beforeOpen
	beforeOpen = swap
	t.Cleanup(func() { beforeOpen = old })
}

func TestIdentifyRefusesAFIFOSwappedInAfterTheCheck(t *testing.T) {
	path := writeFile(t, "hrn", "engine bytes")
	swapBeforeOpen(t, func(p string) {
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := identifyWithin(t, path); err == nil {
		t.Fatal("Identify accepted a FIFO swapped in after the check")
	}
}

func TestIdentifyRefusesALinkSwappedInAfterTheCheck(t *testing.T) {
	path := writeFile(t, "hrn", "engine bytes")
	other := writeFile(t, "other", "engine bytes")
	swapBeforeOpen(t, func(p string) {
		// A link to the very same file must be refused too: Identify promises no links at all.
		if err := os.Rename(p, p+".real"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(p+".real", p); err != nil {
			t.Error(err)
		}
	})
	if err := identifyWithin(t, path); err == nil {
		t.Fatal("Identify followed a link swapped in after the check")
	}
	_ = other
}
