//go:build windows

package ipc

import (
	"context"
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func currentSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

var pipeCheckSID = currentSID
var pipeReadOwnerSID = pipeOwnerSID

func pipePath(cfg Config) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	if err := validateStateDir(cfg.StateDir); err != nil {
		return "", err
	}
	sid, err := currentSID()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\adanext-` + cfg.Name + "-" + sid, nil
}

// listen creates the first instance of an owner-only named pipe.
func listen(cfg Config) (net.Listener, error) {
	path, err := pipePath(cfg)
	if err != nil {
		return nil, err
	}
	sid, err := currentSID()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSDDL(sid)})
}

func pipeSDDL(sid string) string { return "O:" + sid + "D:P(A;;GA;;;" + sid + ")" }

// Dial connects only when the pipe server process belongs to the current user.
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	path, err := pipePath(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeServer(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func verifyPipeServer(conn net.Conn) error {
	server, err := pipeServerSID(conn)
	if err != nil {
		return err
	}
	owner, err := pipeReadOwnerSID(conn)
	if err != nil {
		return err
	}
	current, err := pipeCheckSID()
	if err != nil {
		return err
	}
	if err := allowPipeServerSID(server, current); err != nil {
		return err
	}
	return allowPipeOwnerSID(owner, current)
}

func pipeOwnerSID(conn net.Conn) (string, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", fmt.Errorf("ipc: pipe connection has no Windows handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("ipc: get pipe owner: %w", err)
	}
	if sd == nil {
		return "", fmt.Errorf("ipc: pipe has no security descriptor")
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", fmt.Errorf("ipc: read pipe owner: %w", err)
	}
	if owner == nil {
		return "", fmt.Errorf("ipc: pipe has no owner")
	}
	return owner.String(), nil
}

func pipeServerSID(conn net.Conn) (string, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", fmt.Errorf("ipc: pipe connection has no Windows handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return "", fmt.Errorf("ipc: get pipe server PID: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("ipc: open pipe server: %w", err)
	}
	defer windows.CloseHandle(h)
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("ipc: get pipe server token: %w", err)
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("ipc: get pipe server user: %w", err)
	}
	return u.User.Sid.String(), nil
}

func allowPipeServerSID(server, current string) error {
	if server != current {
		return fmt.Errorf("ipc: pipe server belongs to a different user")
	}
	return nil
}

func allowPipeOwnerSID(owner, current string) error {
	if owner != current {
		return fmt.Errorf("ipc: pipe owner belongs to a different user")
	}
	return nil
}
