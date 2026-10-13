//go:build windows

package ipc

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestPipeDescriptorGrantsOnlyTheCurrentUser(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "descriptor"}
	listener, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-done
		}
	}()
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	f := conn.(interface{ Fd() uintptr })
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL is not protected: control=%v err=%v", control, err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatalf("want exactly one ACE: acl=%v err=%v", acl, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	// Named pipes map GENERIC_ALL to FILE_ALL_ACCESS in the stored ACE.
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != 0x1f01ff || aceSID.String() != sid {
		t.Fatalf("unexpected ACE: type=%d mask=%x sid=%s", ace.Header.AceType, ace.Mask, aceSID.String())
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != sid {
		t.Fatalf("unexpected owner: %v, %v", owner, err)
	}
}
