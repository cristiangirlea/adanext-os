//go:build fakeengine && windows

package main

import (
	"encoding/json"
	"os"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

func job(value string) error {
	if value == "self" {
		value = strconv.Itoa(os.Getpid())
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var inJob int32
	ok, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob").Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&inJob)))
	if ok == 0 {
		return callErr
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// The helper inherits its caller's job. Record those limits separately
	// from the target's membership; a target may be in a different nested job.
	queryErr := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil)
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"pid": pid, "in_job": inJob != 0, "caller_job_limits": limits.BasicLimitInformation.LimitFlags, "query_ok": queryErr == nil})
}
