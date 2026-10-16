//go:build fakeengine && windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
)

func creationTime() (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)), nil
}
