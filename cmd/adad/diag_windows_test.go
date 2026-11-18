//go:build windows

package main

// Diagnostic only, never merged: times the broker's steps on a hosted Windows runner.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const diagHead = `$ErrorActionPreference='Stop'
$sw=[Diagnostics.Stopwatch]::StartNew()
function say($m){ [Console]::Out.WriteLine(("{0,7} ms  {1}" -f $sw.ElapsedMilliseconds,$m)); [Console]::Out.Flush() }
say ("powershell " + $PSVersionTable.PSVersion)
say ("winmgmt " + (Get-Service Winmgmt).Status)
`

const diagCIM = `$environment=[string[]]([Environment]::GetEnvironmentVariables('Process').GetEnumerator() | ForEach-Object { $_.Key+'='+$_.Value })
say ("environment entries " + $environment.Length)
$startup=New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0;CreateFlags=[uint32]0x08000400;EnvironmentVariables=$environment}
say "startup instance made (CimCmdlets loaded)"
$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='cmd.exe /c exit 0';CurrentDirectory='C:\';ProcessStartupInformation=$startup}
say ("cim create rv=" + $r.ReturnValue + " pid=" + $r.ProcessId)
`

const diagCIMNoEnv = `$startup=New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0;CreateFlags=[uint32]0x08000400}
say "startup instance made (CimCmdlets loaded)"
$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='cmd.exe /c exit 0';CurrentDirectory='C:\';ProcessStartupInformation=$startup}
say ("cim create rv=" + $r.ReturnValue + " pid=" + $r.ProcessId)
`

const diagWMIClass = `$s=([wmiclass]'Win32_ProcessStartup').CreateInstance()
$s.ShowWindow=0
$s.CreateFlags=0x08000400
say "startup instance made (System.Management)"
$r=([wmiclass]'Win32_Process').Create('cmd.exe /c exit 0','C:\',$s)
say ("wmiclass create rv=" + $r.ReturnValue + " pid=" + $r.ProcessId)
`

const diagNothing = `say "nothing else"
`

func diagRun(w io.Writer, label, body string) {
	units := utf16.Encode([]rune(diagHead + body))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[i*2:], unit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ps := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	ps.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var out bytes.Buffer
	ps.Stdout, ps.Stderr = &out, &out
	ps.WaitDelay = 5 * time.Second
	start := time.Now()
	err := ps.Run()
	fmt.Fprintf(w, "=== %s: total %v err=%v\n%s\n", label, time.Since(start).Round(time.Millisecond), err, out.String())
}

func diagJob(allowBreakaway bool) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if allowBreakaway {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	return windows.AssignProcessToJobObject(job, windows.CurrentProcess())
}

func TestDiagChild(t *testing.T) {
	mode := os.Getenv("ADA_DIAG_MODE")
	if mode == "" {
		return
	}
	out, err := os.OpenFile(os.Getenv("ADA_DIAG_OUT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	where, what, _ := strings.Cut(mode, "-")
	switch where {
	case "deny":
		err = diagJob(false)
	case "allow":
		err = diagJob(true)
	}
	if err != nil {
		fmt.Fprintf(out, "=== %s: job: %v\n", mode, err)
		return
	}
	switch what {
	case "bare":
		diagRun(out, mode, diagNothing)
	case "cim":
		diagRun(out, mode, diagCIM)
	case "cimnoenv":
		diagRun(out, mode, diagCIMNoEnv)
	case "wmiclass":
		diagRun(out, mode, diagWMIClass)
	case "real", "realdrop":
		if what == "realdrop" {
			dropInheritedStdio()
		}
		brokerWait = 90 * time.Second
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBrokerMarker$")
		cmd.Dir = detachDir()
		start := time.Now()
		err := brokerDetached(cmd)
		fmt.Fprintf(out, "=== %s: brokerDetached total %v err=%v\n\n", mode, time.Since(start).Round(time.Millisecond), err)
	}
}

func TestDiagBroker(t *testing.T) {
	if os.Getenv("ADA_DIAG") == "" {
		t.Skip("diagnostic")
	}
	out := filepath.Join(t.TempDir(), "diag.txt")
	modes := []string{
		"nojob-bare", "nojob-cim", "nojob-cim",
		"deny-bare", "deny-cim", "deny-cim", "deny-cimnoenv", "deny-wmiclass",
		"allow-cim", "nojob-wmiclass",
		"nojob-real", "deny-real", "deny-realdrop",
	}
	for _, mode := range modes {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiagChild$")
		cmd.Env = append(os.Environ(), "ADA_DIAG_MODE="+mode, "ADA_DIAG_OUT="+out)
		cmd.WaitDelay = 5 * time.Second
		start := time.Now()
		b, err := cmd.CombinedOutput()
		cancel()
		t.Logf("child %s: %v err=%v %s", mode, time.Since(start).Round(time.Millisecond), err, bytes.TrimSpace(b))
	}
	content, _ := os.ReadFile(out)
	t.Logf("\n%s", content)
}
