//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func socketDir(cfg Config) string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "adanext")
	}
	return filepath.Join(cfg.StateDir, "run")
}

func privateSocketDir(cfg Config, create bool) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	dir := socketDir(cfg)
	if create {
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return "", err
			}
		} else if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("ipc: socket directory %q must be a real owner-only directory", dir)
	}
	return dir, nil
}

// listen binds an owner-only Unix socket in a verified private directory.
func listen(cfg Config) (net.Listener, error) {
	dir, err := privateSocketDir(cfg, true)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, cfg.Name+".sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("ipc: existing endpoint is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	return &ownerListener{UnixListener: l}, nil
}

type ownerListener struct{ *net.UnixListener }

func (l *ownerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.UnixListener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(conn)
		if err == nil && allowPeerUID(uid) {
			return conn, nil
		}
		conn.Close()
	}
}

func allowPeerUID(uid uint32) bool { return uid == uint32(os.Getuid()) }

// Dial checks the socket directory and server UID before returning a connection.
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	dir, err := privateSocketDir(cfg, false)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, cfg.Name+".sock"))
	if err != nil {
		return nil, err
	}
	uid, err := peerUID(conn.(*net.UnixConn))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ipc: get socket server UID: %w", err)
	}
	if !allowPeerUID(uid) {
		conn.Close()
		return nil, fmt.Errorf("ipc: socket server belongs to a different user")
	}
	return conn, nil
}
