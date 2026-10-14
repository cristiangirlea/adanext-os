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
	return filepath.Join(cfg.StateDir, "run")
}

func privateSocketDir(cfg Config, create bool) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	dir := socketDir(cfg)
	if create {
		if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
			return "", err
		}
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	if err := validateStateDir(cfg.StateDir); err != nil {
		return "", err
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
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	path := filepath.Join(socketDir(cfg), cfg.Name+".sock")
	if err := validateSocketPath(path); err != nil {
		return nil, err
	}
	if err := peerChecksSupported(); err != nil {
		return nil, err
	}
	dir, err := privateSocketDir(cfg, true)
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, cfg.Name+".sock")
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
	l.SetUnlinkOnClose(false)
	boundInfo, err := os.Lstat(path)
	if err != nil {
		l.Close()
		return nil, err
	}
	owner := &ownerListener{UnixListener: l, path: path, boundInfo: boundInfo}
	if err := os.Chmod(path, 0600); err != nil {
		owner.Close()
		return nil, err
	}
	return owner, nil
}

type ownerListener struct {
	*net.UnixListener
	path       string
	boundInfo  os.FileInfo
	removePath func(string) error
}

func (l *ownerListener) Close() error {
	info, err := os.Lstat(l.path)
	if os.IsNotExist(err) {
		return l.UnixListener.Close()
	}
	if err != nil {
		closeErr := l.UnixListener.Close()
		if closeErr != nil {
			return closeErr
		}
		return err
	}
	var removeErr error
	if os.SameFile(l.boundInfo, info) {
		remove := l.removePath
		if remove == nil {
			remove = os.Remove
		}
		removeErr = remove(l.path)
	}
	closeErr := l.UnixListener.Close()
	if removeErr != nil {
		return removeErr
	}
	return closeErr
}

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

var currentPeerUID = func() uint32 { return uint32(os.Getuid()) }

func allowPeerUID(uid uint32) bool { return uid == currentPeerUID() }

func validateSocketPath(path string) error {
	if len(path) >= socketPathLimit() {
		return fmt.Errorf("ipc: socket path %q exceeds the %d-byte sun_path limit", path, socketPathLimit())
	}
	return nil
}

// Dial checks the socket directory and server UID before returning a connection.
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	path := filepath.Join(socketDir(cfg), cfg.Name+".sock")
	if err := validateSocketPath(path); err != nil {
		return nil, err
	}
	if err := peerChecksSupported(); err != nil {
		return nil, err
	}
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
