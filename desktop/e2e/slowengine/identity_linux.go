//go:build fakeengine && linux

package main

import (
	"fmt"
	"os"
	"strings"
)

func creationTime() (string, error) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+2:])
	if len(fields) < 20 {
		return "", fmt.Errorf("missing process creation time")
	}
	return fields[19], nil
}
