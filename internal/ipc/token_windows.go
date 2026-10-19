//go:build windows

package ipc

import (
	"crypto/rand"
	"encoding/hex"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"unsafe"
)

func createTokenFile(dir string) (*os.File, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return createOwnerOnly(filepath.Join(dir, ".adad-token-"+hex.EncodeToString(nonce)), 0)
}

// createStartupFile lets the owner read the log while a hung start still holds it.
func createStartupFile(name string) (*os.File, error) {
	return createOwnerOnly(name, windows.FILE_SHARE_READ)
}

// createOwnerOnly creates a new file whose DACL grants only the current user; share is
// what others may open it for while it is held.
func createOwnerOnly(name string, share uint32) (*os.File, error) {
	sid, err := currentSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL(sid))
	if err != nil {
		return nil, err
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := windows.CreateFile(ptr, windows.GENERIC_WRITE, share, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

// The token itself carries an owner-only DACL on Windows.
func checkTokenDir(string) error { return nil }

func readToken(dir string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, "adad.token")) }
