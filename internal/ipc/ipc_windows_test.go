//go:build windows

package ipc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	wantSID, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID, err := pipeOwnerSID(conn)
	if err != nil || ownerSID != wantSID {
		t.Fatalf("pipe owner = %q, want %q: %v", ownerSID, wantSID, err)
	}
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
	want := "O:S-1-5-21-123-456-789-1001D:P(A;;GA;;;S-1-5-21-123-456-789-1001)"
	if got := pipeSDDL(sid); got != want {
		t.Fatalf("pipeSDDL(%q) = %q, want %q", sid, got, want)
	}
}

func TestPipeOwnerSIDAllowsOnlyTheCurrentUser(t *testing.T) {
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := allowPipeOwnerSID(sid, sid); err != nil {
		t.Fatal(err)
	}
	if err := allowPipeOwnerSID("S-1-5-18", sid); err == nil {
		t.Fatal("another pipe owner was accepted")
	}
}

func TestDialRefusesAPipeWhenTheExpectedSIDChanges(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "sid-check"}
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
	current := pipeCheckSID
	done := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-done
		}
	}()
	defer close(done)
	pipeCheckSID = func() (string, error) { return "S-1-5-18", nil }
	defer func() { pipeCheckSID = current }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "different user") {
		t.Fatalf("Dial accepted mismatched pipe: %v", err)
	}
}

func TestDialRefusesAPipeWhenOnlyTheOwnerSIDChanges(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "owner-check"}
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
	readOwner := pipeReadOwnerSID
	pipeReadOwnerSID = func(net.Conn) (string, error) { return "S-1-5-18", nil }
	defer func() { pipeReadOwnerSID = readOwner }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "pipe owner belongs to a different user") {
		t.Fatalf("Dial accepted mismatched pipe owner: %v", err)
	}
}

func TestALiveWindowsHolderReturnsWithoutStaleRetry(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "helper"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCoordinatorLock$")
	cmd.Env = append(os.Environ(), "ADANEXT_IPC_HELPER=1", "ADANEXT_IPC_STATE="+cfg.StateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("helper ready: %q %v", ready, err)
	}
	start := time.Now()
	lock, err := Acquire(cfg)
	elapsed := time.Since(start)
	if lock != nil {
		lock.Release()
	}
	var running *AlreadyRunningError
	if !errors.As(err, &running) || running.PID != cmd.Process.Pid {
		t.Fatalf("Acquire = %v, want live holder PID %d", err, cmd.Process.Pid)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("live holder took %v, want under 500ms", elapsed)
	}
}

func TestWindowsRetryReportsTheHolderAfterTheRecordChanges(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "helper"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCoordinatorLock$")
	cmd.Env = append(os.Environ(), "ADANEXT_IPC_HELPER=1", "ADANEXT_IPC_REPLACE_RECORD=1", "ADANEXT_IPC_STATE="+cfg.StateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	output := bufio.NewReader(stdout)
	if signal, err := output.ReadString('\n'); err != nil || signal != "fake\n" {
		t.Fatalf("helper fake record: %q %v", signal, err)
	}
	type acquireResult struct {
		lock *Lock
		err  error
	}
	result := make(chan acquireResult, 1)
	start := time.Now()
	go func() {
		lock, err := Acquire(cfg)
		result <- acquireResult{lock, err}
	}()
	// Observe the retry before restoring the record, so the returned PID
	// must come from Acquire's final read rather than its initial read.
	stack := make([]byte, 64<<10)
	enteredRetry := time.NewTimer(time.Second)
	defer enteredRetry.Stop()
	for {
		n := runtime.Stack(stack, true)
		if strings.Contains(string(stack[:n]), "ipc.retryStaleLock(") {
			break
		}
		select {
		case got := <-result:
			if got.lock != nil {
				got.lock.Release()
			}
			t.Fatalf("Acquire returned before retrying: %v", got.err)
		case <-enteredRetry.C:
			t.Fatal("Acquire did not enter the stale-lock retry")
		default:
			runtime.Gosched()
		}
	}
	if _, err := io.WriteString(stdin, "restore\n"); err != nil {
		t.Fatal(err)
	}
	if signal, err := output.ReadString('\n'); err != nil || signal != "restored\n" {
		t.Fatalf("helper restored record: %q %v", signal, err)
	}
	var got acquireResult
	select {
	case got = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire exceeded the 2s retry bound plus 1s margin")
	}
	elapsed := time.Since(start)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if got.lock != nil {
		got.lock.Release()
	}
	var running *AlreadyRunningError
	if !errors.As(got.err, &running) || running.PID != cmd.Process.Pid {
		t.Fatalf("Acquire = %v, want current holder PID %d", got.err, cmd.Process.Pid)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Acquire took %v, want at most 2s retry bound plus 1s margin", elapsed)
	}
}

func TestAStaleWindowsLockIsRetriedUntilItsKernelLockClears(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "helper"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCoordinatorLock$")
	cmd.Env = append(os.Environ(), "ADANEXT_IPC_HELPER=1", "ADANEXT_IPC_STALE_RECORD=1", "ADANEXT_IPC_STATE="+cfg.StateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("helper ready: %q %v", ready, err)
	}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatalf("Acquire did not retry a stale lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAStateDirectorySymlinkIsRefusedOnWindows(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	lock, err := Acquire(Config{StateDir: path, Name: "link"})
	if lock != nil {
		lock.Release()
	}
	if err == nil {
		t.Fatal("Acquire accepted a state directory symlink")
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
