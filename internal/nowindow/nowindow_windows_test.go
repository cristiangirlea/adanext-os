package nowindow

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

var getConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// TestHelperConsoleWindow is the probe: it prints its console window handle.
func TestHelperConsoleWindow(t *testing.T) {
	if os.Getenv("ADA_NOWINDOW_HELPER") != "probe" {
		t.Skip("helper process")
	}
	hwnd, _, _ := getConsoleWindow.Call()
	fmt.Printf("hwnd=%d\n", hwnd)
}

// TestHelperDetachedParent stands in for adad: it has no console, and starts
// the probe, through Set when ADA_NOWINDOW_SET is 1, passing on its output.
func TestHelperDetachedParent(t *testing.T) {
	if os.Getenv("ADA_NOWINDOW_HELPER") != "parent" {
		t.Skip("helper process")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperConsoleWindow$")
	cmd.Env = append(os.Environ(), "ADA_NOWINDOW_HELPER=probe")
	if os.Getenv("ADA_NOWINDOW_SET") == "1" {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		Set(cmd)
	}
	out, err := cmd.Output()
	if err != nil {
		fmt.Printf("probe: %v\n", err)
	}
	os.Stdout.Write(out)
}

// probeFromADetachedParent reports the console window the probe sees when a
// console-less parent starts it.
func probeFromADetachedParent(t *testing.T, set bool) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDetachedParent$")
	flag := "0"
	if set {
		flag = "1"
	}
	cmd.Env = append(os.Environ(), "ADA_NOWINDOW_HELPER=parent", "ADA_NOWINDOW_SET="+flag)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("detached parent: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "hwnd="); ok {
			return v
		}
	}
	t.Fatalf("the probe printed no handle: %q", out)
	return ""
}

func TestAChildOfAConsolelessParentStartedWithSetHasNoConsoleWindow(t *testing.T) {
	if hwnd := probeFromADetachedParent(t, true); hwnd != "0" {
		t.Fatalf("console window = %s, want none", hwnd)
	}
}

func TestSetKeepsTheCreationFlagsAlreadySet(t *testing.T) {
	cmd := exec.Command("unused")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	Set(cmd)
	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	if got := cmd.SysProcAttr.CreationFlags; got != want {
		t.Fatalf("creation flags = %#x, want %#x", got, want)
	}
}
