//go:build windows

package source

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOnlyNameSurrogateReparseTagsAreLinks(t *testing.T) {
	t.Parallel()
	for tag, want := range map[uint32]bool{
		0xA000000C: true,  // IO_REPARSE_TAG_SYMLINK
		0xA0000003: true,  // IO_REPARSE_TAG_MOUNT_POINT, a junction
		0xA000001D: true,  // IO_REPARSE_TAG_LX_SYMLINK
		0x9000001A: false, // IO_REPARSE_TAG_CLOUD, OneDrive
		0x9000601A: false, // IO_REPARSE_TAG_CLOUD_6
		0x80000013: false, // IO_REPARSE_TAG_DEDUP
		0x80000023: false, // IO_REPARSE_TAG_AF_UNIX
	} {
		if got := isNameSurrogate(tag); got != want {
			t.Errorf("isNameSurrogate(%#x) = %v, want %v", tag, got, want)
		}
	}
}

func TestOpenSharedRefusesADeviceADirectoryAndALink(t *testing.T) {
	t.Parallel()
	if _, _, err := readFile("NUL", maxFileSize); !errors.Is(err, errNotRegular) {
		t.Fatalf("NUL: %v", err)
	}
	dir := tempDir(t)
	if _, _, err := readFile(dir, maxFileSize); err == nil {
		t.Fatal("a directory was read")
	}
	junction := filepath.Join(tempDir(t), "j")
	makeLink(t, dir, junction)
	if _, _, err := readFile(junction, maxFileSize); err == nil {
		t.Fatal("a junction was read")
	}
	file := filepath.Join(dir, "file")
	write(t, file, "x\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("a file symlink needs Developer Mode or a privilege: %v", err)
	}
	if _, _, err := readFile(link, maxFileSize); !errors.Is(err, errLink) {
		t.Fatalf("a file symlink: %v", err)
	}
}
