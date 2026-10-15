//go:build windows

package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestOpenRefusesDirectoryJunctions(t *testing.T) {
	for _, name := range []string{"store", "tmp"} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			dir := filepath.Join(t.TempDir(), "artifacts")
			link := dir
			if name == "tmp" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(dir, "tmp")
			}
			output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
			if err != nil {
				t.Fatalf("create junction: %v: %s", err, output)
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeIrregular == 0 {
				t.Fatalf("junction did not exercise ModeIrregular: %v %v", info, err)
			}
			if _, err := Open(dir); err == nil {
				t.Fatal("accepted directory junction")
			}
		})
	}
}

func TestOpenRefusesADirectorySymlinkWhenWindowsPermitsOne(t *testing.T) {
	for _, name := range []string{"store", "tmp"} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			dir := filepath.Join(t.TempDir(), "artifacts")
			link := dir
			if name == "tmp" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(dir, "tmp")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("creating a directory symlink is not permitted: %v", err)
			}
			if _, err := Open(dir); err == nil {
				t.Fatal("accepted directory symlink")
			}
		})
	}
}

func TestRepairWhileAReaderHoldsTheBlobIsBusyNotDiskFull(t *testing.T) {
	s := testStore(t)
	data := []byte("original")
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := s.Open(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, err = s.Write(context.Background(), bytes.NewReader(data), 100)
	if !errors.Is(err, ErrBusy) || errors.Is(err, ErrDiskFull) {
		t.Fatalf("repair under an open reader: %v", err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Write(context.Background(), bytes.NewReader(data), 100); err != nil || got != ref {
		t.Fatalf("retry after the reader closed: %+v %v", got, err)
	}
	if repaired, err := os.ReadFile(path); err != nil || !bytes.Equal(repaired, data) {
		t.Fatalf("repaired content: %q %v", repaired, err)
	}
}

func TestOpenBlobRefusesAReparsePoint(t *testing.T) {
	t.Run("junction", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "blob")
		output, err := exec.Command("cmd", "/c", "mklink", "/J", link, t.TempDir()).CombinedOutput()
		if err != nil {
			t.Fatalf("create junction: %v: %s", err, output)
		}
		if f, err := openBlob(link); err == nil {
			f.Close()
			t.Fatal("opened a junction")
		}
	})
	t.Run("file symlink", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "blob")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("creating a file symlink is not permitted: %v", err)
		}
		f, err := openBlob(link)
		if err == nil {
			f.Close()
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open file symlink: %v", err)
		}
	})
}

func currentUserSID(t *testing.T) *windows.SID {
	t.Helper()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid
}

func TestDirectoriesAndBlobsGrantOnlyTheCurrentUser(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		t.Skipf("cannot set an owner-only DACL here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Write(context.Background(), strings.NewReader("private"), 100)
	if err != nil {
		t.Fatal(err)
	}
	user := currentUserSID(t)
	fanout := filepath.Join(s.dir, ref.Hash[:2])
	for _, path := range []string{s.dir, filepath.Join(s.dir, "tmp"), fanout, filepath.Join(fanout, ref.Hash)} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		owner, _, err := sd.Owner()
		if err != nil || !owner.Equals(user) {
			t.Fatalf("%s is owned by %v: %v", path, owner, err)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount == 0 {
			t.Fatalf("%s has no DACL with entries: %v %v", path, dacl, err)
		}
		for i := range dacl.AceCount {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
				t.Fatal(err)
			}
			principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !principal.Equals(user) {
				t.Fatalf("%s grants %s (ACE type %d)", path, principal, ace.Header.AceType)
			}
		}
	}
}
