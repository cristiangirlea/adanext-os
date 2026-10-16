//go:build windows

package source

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
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
	if _, _, err := readFile(dir, maxFileSize); !errors.Is(err, errNotRegular) {
		t.Fatalf("a directory: %v", err)
	}
	junction := filepath.Join(tempDir(t), "j")
	makeLink(t, dir, junction)
	if _, _, err := readFile(junction, maxFileSize); !errors.Is(err, errLink) {
		t.Fatalf("a junction: %v", err)
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

func TestAReparsePointIsALinkByItsTag(t *testing.T) {
	t.Parallel()
	dir, target := tempDir(t), tempDir(t)
	file := filepath.Join(dir, "file")
	write(t, file, "x\n")
	junction := filepath.Join(dir, "j")
	makeLink(t, target, junction)
	info, err := os.Lstat(junction)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		path string
		info os.FileInfo
		want bool
	}{
		"no reparse tag":       {file, fileInfo{fs.ModeIrregular}, false},
		"an unreadable tag":    {filepath.Join(dir, "missing"), fileInfo{fs.ModeIrregular}, true},
		"a junction":           {junction, info, true},
		"a symlink by mode":    {file, fileInfo{fs.ModeSymlink}, true},
		"a plain file by mode": {file, fileInfo{}, false},
	} {
		if got := isLink(c.path, c.info); got != c.want {
			t.Errorf("%s: isLink = %v, want %v", name, got, c.want)
		}
	}
	if got, ok := resolve(dir, junction, false, true); !ok || !strings.EqualFold(got, target) {
		t.Fatalf("resolve(junction) = %q, %v", got, ok)
	}
}

func TestALinkReadlinkCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	write(t, filepath.Join(dir, "file"), "x\n")
	lx := filepath.Join(dir, "lx")
	makeLXSymlink(t, lx, "file")
	info, err := os.Lstat(lx)
	if err != nil {
		t.Fatal(err)
	}
	if !isLink(lx, info) {
		t.Fatalf("an LX symlink (mode %v) is not a link", info.Mode())
	}
	if err := noLinks(dir, lx); !errors.Is(err, errLink) {
		t.Fatalf("noLinks = %v", err)
	}
	if got, ok := resolve(dir, lx, false, true); ok {
		t.Fatalf("resolve = %q, ok", got)
	}
}

func TestReparseTagReadsAJunctionsTag(t *testing.T) {
	t.Parallel()
	junction := filepath.Join(tempDir(t), "j")
	makeLink(t, tempDir(t), junction)
	h := openReparse(t, junction, windows.FILE_READ_ATTRIBUTES)
	if tag, err := reparseTag(h); err != nil || tag != 0xA0000003 {
		t.Fatalf("reparseTag = %#x, %v", tag, err)
	}
}

// openReparse opens path with openReparsePoint, closed at the test's end.
func openReparse(t *testing.T, path string, access uint32) windows.Handle {
	t.Helper()
	h, err := openReparsePoint(path, access)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

// makeLXSymlink makes a WSL symlink, a name surrogate Go's Readlink cannot read.
func makeLXSymlink(t *testing.T, path, target string) {
	t.Helper()
	write(t, path, "")
	data := append(binary.LittleEndian.AppendUint32(nil, 2), target...) // version 2, UTF-8 target
	buf := binary.LittleEndian.AppendUint32(nil, 0xA000001D)            // IO_REPARSE_TAG_LX_SYMLINK
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(data)))
	buf = append(binary.LittleEndian.AppendUint16(buf, 0), data...)
	var n uint32
	const fsctlSetReparsePoint = 0x900A4
	if err := windows.DeviceIoControl(openReparse(t, path, windows.GENERIC_WRITE), fsctlSetReparsePoint, &buf[0], uint32(len(buf)), nil, 0, &n, nil); err != nil {
		t.Skipf("setting an LX symlink tag: %v", err)
	}
}
