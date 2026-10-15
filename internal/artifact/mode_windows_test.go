//go:build windows

package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
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
		f, err := openBlob(link)
		if err == nil {
			f.Close()
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open junction: %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		f, err := openBlob(t.TempDir())
		if err == nil {
			f.Close()
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open directory: %v", err)
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
	for _, path := range []string{s.dir, filepath.Join(s.dir, "tmp"), fanout} {
		assertOwnerOnlyDirectoryDACL(t, path, user)
	}
}

// assertOwnerOnlyDirectoryDACL checks the exact descriptor makePrivateDirectory
// leaves: owned by user, protected from inheritance, one allow ACE for user with
// FILE_ALL_ACCESS that children inherit.
func assertOwnerOnlyDirectoryDACL(t *testing.T, path string, user *windows.SID) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(user) {
		t.Fatalf("%s is owned by %v: %v", path, owner, err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s DACL is not protected (control %#x): %v", path, control, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("%s has %v ACEs, want one: %v", path, dacl, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !principal.Equals(user) {
		t.Fatalf("%s grants %s (ACE type %d)", path, principal, ace.Header.AceType)
	}
	if ace.Mask != fileAllAccess {
		t.Fatalf("%s grants %#x, want FILE_ALL_ACCESS %#x", path, ace.Mask, fileAllAccess)
	}
	if ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		t.Fatalf("%s ACE flags %#x, want object and container inheritance", path, ace.Header.AceFlags)
	}
}

func TestPrivateDirectoryDACLHoldsWhileTheCollectorRuns(t *testing.T) {
	user := currentUserSID(t)
	root := t.TempDir()
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	const rounds = 200
	for i := range rounds {
		dir := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		sid, err := user.Copy()
		if err != nil {
			t.Fatal(err)
		}
		err = makePrivateDirectoryForSID(dir, sid)
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			t.Skipf("cannot set an owner-only DACL here: %v", err)
		}
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		_ = make([]byte, 64<<10)
	}
	close(stop)
	<-collected
	for i := range rounds {
		assertOwnerOnlyDirectoryDACL(t, filepath.Join(root, strconv.Itoa(i)), user)
	}
}

func TestReusingPrivateDirectoriesDoesNotRewriteTheirDACL(t *testing.T) {
	s := testStore(t)
	data := []byte("stored twice")
	if _, err := s.Write(context.Background(), bytes.NewReader(data), 100); err != nil {
		t.Fatal(err)
	}
	calls := 0
	original := setDirectoryDACL
	setDirectoryDACL = func(h windows.Handle, acl *windows.ACL) error {
		calls++
		return original(h, acl)
	}
	t.Cleanup(func() { setDirectoryDACL = original })
	if _, err := s.Write(context.Background(), bytes.NewReader(data), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s.dir); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("rewrote the DACL of an already private directory %d times", calls)
	}
}

func TestExtendedPathPrefixesAbsolutePathsOnce(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\store\ab\blob`, `\\?\C:\store\ab\blob`},
		{`C:/store/ab/../blob`, `\\?\C:\store\blob`},
		{`\\server\share\store\blob`, `\\?\UNC\server\share\store\blob`},
		{`\\?\C:\store\blob`, `\\?\C:\store\blob`},
		{`\\?\UNC\server\share\blob`, `\\?\UNC\server\share\blob`},
		{`\\.\pipe\store`, `\\.\pipe\store`},
		{`store\blob`, `store\blob`},
	} {
		if got := extendedPath(tc.in); got != tc.want {
			t.Errorf("extendedPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestABlobBeyondMaxPathIsWrittenOpenedReusedAndRepaired(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("a", 200), "artifacts")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("beyond MAX_PATH")
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	if len(path) <= 260 {
		t.Fatalf("blob path is %d characters, not beyond MAX_PATH", len(path))
	}
	reader, err := s.Open(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %q: %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Write(context.Background(), bytes.NewReader(data), 100); err != nil || again != ref {
		t.Fatalf("reuse: %+v %v", again, err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains after reuse")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if repaired, err := s.Write(context.Background(), bytes.NewReader(data), 100); err != nil || repaired != ref {
		t.Fatalf("repair: %+v %v", repaired, err)
	}
	if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, data) {
		t.Fatalf("repaired content %q: %v", content, err)
	}
}
