//go:build !windows

package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportRootPermissions(t *testing.T) {
	root := permissionTestChild(t, permissionTestTop(t), "root")
	ancestor := filepath.Join(root, "ancestor")
	child := filepath.Join(ancestor, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ancestor, 0700) })
	for _, mode := range []os.FileMode{0711, 0311} {
		if err := os.Chmod(ancestor, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BrowseImportRoot(root, "ancestor/child"); err != nil {
			t.Fatalf("ancestor mode %04o: %v", mode, err)
		}
	}
	if err := os.Chmod(ancestor, 0777); err != nil {
		t.Fatal(err)
	}
	_, _, err := BrowseImportRoot(root, "ancestor/child")
	assertRootCode(t, err, "root_writable_by_others")
	_, err = CanonicalImportRoot(ancestor)
	assertRootCode(t, err, "root_writable_by_others")
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	_, _, err = BrowseImportRoot(root, "")
	assertRootCode(t, err, "root_writable_by_others")
}

func TestImportRootChecksUnixAncestors(t *testing.T) {
	parent := permissionTestChild(t, permissionTestTop(t), "parent")
	root := permissionTestChild(t, parent, "root")
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	_, err := CanonicalImportRoot(root)
	assertRootCode(t, err, "root_writable_by_others")
	_, _, err = BrowseImportRoot(root, "")
	assertRootCode(t, err, "root_writable_by_others")
	if err := os.Chmod(parent, 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalImportRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BrowseImportRoot(root, ""); err != nil {
		t.Fatal(err)
	}
}

func TestChooserAllowsWritableMetadataWithinRoot(t *testing.T) {
	testChooserChecksAlternateBoundaries(t, func(path string) {
		if err := os.Chmod(path, 0777); err != nil {
			t.Fatal(err)
		}
	})
}
