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

	"golang.org/x/sys/windows"
)

// Node's detached spawn gives us a separate console/process group but can still
// inherit Electron's job, and on Windows it always hands over Electron's inheritable
// handles, such as the pipes whoever launched Electron reads. Before opening state,
// re-exec, in or out of a job: the serving process gets only its own null stdio, and
// this one exits, so nothing the desktop held outlives it. Git Bash's job forbids
// breakaway; there the same-user WMI process broker starts us.
func detachFromJob(args []string) (bool, error) {
	return detachWith(args, func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				dropInheritedStdio()
				return brokerDetached(cmd)
			}
			return err
		}
		return cmd.Process.Release()
	})
}

// dropInheritedStdio lets go of the launcher's stdio before the broker, which can take
// seconds, so whoever reads those pipes is not held until it returns. The standard
// handles then point at NUL; what this process still reports goes to the start-up log.
func dropInheritedStdio() {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	closed := map[uintptr]bool{}
	for _, std := range []struct {
		id   uint32
		file **os.File
	}{{windows.STD_INPUT_HANDLE, &os.Stdin}, {windows.STD_OUTPUT_HANDLE, &os.Stdout}, {windows.STD_ERROR_HANDLE, &os.Stderr}} {
		windows.SetStdHandle(std.id, windows.Handle(null.Fd()))
		// One handle may serve two streams; closing it twice could close a reused value.
		if handle := (*std.file).Fd(); !closed[handle] {
			closed[handle] = true
			(*std.file).Close()
		}
		*std.file = null
	}
}

// beforeBroker is a test seam that runs just before the broker starts; nil outside tests.
var beforeBroker func()

// Win32_Process.Create does not inherit the caller's job. No elevation, service,
// scheduled task or policy change is involved. Configuration is argv and the
// owner's environment is passed in memory, never on argv or in a log/file.
func brokerDetached(cmd *exec.Cmd) error {
	quoted := make([]string, len(cmd.Args))
	for i, arg := range cmd.Args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	command := base64.StdEncoding.EncodeToString([]byte(strings.Join(quoted, " ")))
	dir := base64.StdEncoding.EncodeToString([]byte(cmd.Dir))
	script := `$ErrorActionPreference='Stop'
$line=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + command + `'))
$dir=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + dir + `'))
$environment=[string[]]([Environment]::GetEnvironmentVariables('Process').GetEnumerator() | ForEach-Object { $_.Key+'='+$_.Value })
$startup=New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{ShowWindow=[uint16]0;CreateFlags=[uint32]0x08000400;EnvironmentVariables=$environment}
$result=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$line;CurrentDirectory=$dir;ProcessStartupInformation=$startup}
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
	if beforeBroker != nil {
		beforeBroker()
	}
	if err := broker.Run(); err != nil {
		return fmt.Errorf("Windows process broker could not start adad: %w", err)
	}
	return nil
}

// detachDir is the system drive's root, so the coordinator keeps no directory of the
// launcher's busy; empty if Windows cannot say where it is installed.
func detachDir() string {
	windir, err := windows.GetSystemWindowsDirectory()
	if err != nil || filepath.VolumeName(windir) == "" {
		return ""
	}
	return filepath.VolumeName(windir) + `\`
}

func detachWith(args []string, start func(*exec.Cmd) error) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	dir := detachDir()
	if dir == "" {
		return false, errors.New("cannot find the system drive to detach adad into")
	}
	// Breakaway may leave an enclosing job owned by the session/host. It must
	// not recurse trying to escape those: only the desktop's launch job is ours.
	cmd := exec.Command(executable, append(append([]string{}, args...), "-detach=false")...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := start(cmd); err != nil {
		return false, fmt.Errorf("cannot detach adad from the desktop job (start adad outside that job): %w", err)
	}
	return true, nil
}
