//go:build windows

package ipc

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestTokenDescriptorGrantsOnlyTheOwnerWithoutInheritedACEs(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "token-descriptor"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	l, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f, err := os.Open(filepath.Join(cfg.StateDir, "adad.token"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("token DACL is not protected")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatal("token must have exactly one ACE")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != 0x1f01ff || (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() != sid {
		t.Fatal("token grants another principal access")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != sid {
		t.Fatal("token has another owner")
	}
}
