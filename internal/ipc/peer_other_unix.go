//go:build !windows && !linux

package ipc

import (
	"errors"
	"fmt"
	"net"
	"runtime"
)

func peerChecksSupported() error {
	return fmt.Errorf("ipc: peer credential checks are not supported on %s", runtime.GOOS)
}

func socketPathLimit() int { return 104 }

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("ipc: peer UID verification is unsupported on this Unix system")
}
