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
	return createOwnerOnly(filepath.Join(dir, ".adad-token-"+hex.EncodeToString(nonce)))
}

// createOwnerOnly creates a new file whose DACL grants only the current user.
func createOwnerOnly(name string) (*os.File, error) {
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
	h, err := windows.CreateFile(ptr, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

// The token itself carries an owner-only DACL on Windows.
func checkTokenDir(string) error { return nil }
