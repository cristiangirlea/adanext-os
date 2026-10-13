//go:build linux

package ipc

import (
	"fmt"
	"os"
	"strings"
)

func processCreated(pid int) (string, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", fmt.Errorf("ipc: malformed process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("ipc: process stat has too few fields")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(bootID)) + ":" + fields[19], nil
}
