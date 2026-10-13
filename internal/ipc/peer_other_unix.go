//go:build !windows && !linux

package ipc

import (
	"errors"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("ipc: peer UID verification is unsupported on this Unix system")
}
