package store

import (
	"os/exec"
	"testing"
)

// linkDirectory makes link a junction to the directory target, which needs no
// privilege where a directory symlink does.
func linkDirectory(t *testing.T, target, link string) {
	t.Helper()
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
}
