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
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
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
	owner, defaulted, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != sid || defaulted {
		t.Fatalf("unexpected owner: sid=%v defaulted=%v err=%v", owner, defaulted, err)
	}
}

func TestPipeServerSIDAllowsOnlyTheCurrentUser(t *testing.T) {
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := allowPipeServerSID(sid, sid); err != nil {
		t.Fatalf("current user refused: %v", err)
	}
	for _, other := range []string{"S-1-5-18", "S-1-1-0"} {
		if err := allowPipeServerSID(other, sid); err == nil {
			t.Fatalf("server SID %s was allowed", other)
		}
	}
}

func TestPipeSDDLHasUserOwnerAndProtectedSingleACE(t *testing.T) {
	sid := "S-1-5-21-123-456-789-1001"
	want := "O:" + sid + "D:P(A;;GA;;;" + sid + ")"
	if got := pipeSDDL(sid); got != want {
		t.Fatalf("pipeSDDL(%q) = %q, want %q", sid, got, want)
	}
}

func TestASecondListenOnTheSameNameFails(t *testing.T) {
	firstLock, err := Acquire(Config{StateDir: t.TempDir(), Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	defer firstLock.Release()
	first, err := firstLock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	secondLock, err := Acquire(Config{StateDir: t.TempDir(), Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	defer secondLock.Release()
	second, err := secondLock.Listen()
	if err == nil {
		second.Close()
		t.Fatal("second listener succeeded")
	}
}
