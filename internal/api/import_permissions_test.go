package api

import (
	"os"
	"testing"
	_ "unsafe" // Required by go:linkname.
)

// Go omits source's export_test.go when building the dependency for API tests.
//
//go:linkname importPermissionCheck github.com/cristiangirlea/ada-os/internal/source.importPermissionCheck
var importPermissionCheck func(*os.File, bool, bool) error

// trustImportRoots skips owner, DACL and mode checks for chooser directories.
func trustImportRoots(t *testing.T) {
	t.Helper()
	original := importPermissionCheck
	importPermissionCheck = func(*os.File, bool, bool) error { return nil }
	t.Cleanup(func() { importPermissionCheck = original })
}
