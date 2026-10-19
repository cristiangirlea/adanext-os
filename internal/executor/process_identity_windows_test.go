package executor

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func processIdentity(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d", created.HighDateTime, created.LowDateTime), nil
}

// waitProcessGone waits until the process with this pid and identity has
// exited, so it no longer holds its working directory. A process that is
// already gone (OpenProcess finds no such pid), or whose pid now names another
// process, has exited; any other open error is reported.
func waitProcessGone(pid int, identity string, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return err
	}
	if fmt.Sprintf("%d-%d", created.HighDateTime, created.LowDateTime) != identity {
		return nil
	}
	event, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		return err
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d still running after %s", pid, timeout)
	}
	return nil
}
