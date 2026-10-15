//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Node's detached spawn gives us a separate console/process group but can still
// inherit Electron's job. Before opening state, re-exec outside that job. Git
// Bash's job forbids breakaway; there the same-user WMI process broker starts us.
func detachFromJob(args []string) (bool, error) {
	return detachWith(args, func() (bool, error) {
		var inJob int32
		ok, _, err := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob").Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob)))
		if ok == 0 {
			return false, err
		}
		return inJob != 0, nil
	}, func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return brokerDetached(cmd.Args)
			}
			return err
		}
		return cmd.Process.Release()
	})
}

// Win32_Process.Create does not inherit the caller's job. No elevation, service,
// scheduled task or policy change is involved. Configuration is argv and the
// owner's environment is passed in memory, never on argv or in a log/file.
func brokerDetached(args []string) error {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	command := base64.StdEncoding.EncodeToString([]byte(strings.Join(quoted, " ")))
	script := `$ErrorActionPreference='Stop'
$line=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + command + `'))
$environment=[string[]]([Environment]::GetEnvironmentVariables('Process').GetEnumerator() | ForEach-Object { $_.Key+'='+$_.Value })
$startup=New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0;CreateFlags=[uint32]0x08000400;EnvironmentVariables=$environment}
$result=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$line;ProcessStartupInformation=$startup}
if ($result.ReturnValue -ne 0) { exit 1 }
`
	units := utf16.Encode([]rune(script))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[i*2:], unit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	root := os.Getenv("SystemRoot")
	if !filepath.IsAbs(root) {
		return errors.New("SystemRoot must be absolute for the Windows process broker")
	}
	broker := exec.CommandContext(ctx, filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	broker.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := broker.Run(); err != nil {
		return fmt.Errorf("Windows process broker could not start adad: %w", err)
	}
	return nil
}

func detachWith(args []string, inJob func() (bool, error), start func(*exec.Cmd) error) (bool, error) {
	in, err := inJob()
	if err != nil || !in {
		return false, err
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	// Breakaway may leave an enclosing job owned by the session/host. It must
	// not recurse trying to escape those: only the desktop's launch job is ours.
	cmd := exec.Command(executable, append(append([]string{}, args...), "-detach=false")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := start(cmd); err != nil {
		return false, fmt.Errorf("cannot detach adad from the desktop job (start adad outside that job): %w", err)
	}
	return true, nil
}
