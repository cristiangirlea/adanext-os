//go:build !windows

package store

import (
	"os"
	"testing"
)

// linkDirectory makes link a symlink to the directory target.
func linkDirectory(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
