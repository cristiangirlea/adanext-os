//go:build linux

package ipc

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerChecksSupported() error { return nil }

func socketPathLimit() int { return 108 }

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if controlErr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, controlErr
}
