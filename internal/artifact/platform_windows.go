//go:build windows

package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%s is not a regular directory", path)
	}
	return nil
}

// fileAllAccess is FILE_ALL_ACCESS: every standard and file-specific right.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// makePrivateDirectory gives the directory a protected DACL that grants only
// the current user, so nothing inherited from %LOCALAPPDATA% (SYSTEM,
// Administrators, other accounts) reaches the store. The check and the change
// go through one handle that does not follow reparse points. A directory that
// already carries exactly that descriptor is left alone: setting it again
// would push the inheritable entry down onto every blob below.
func makePrivateDirectory(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	return makePrivateDirectoryForSID(path, user.User.Sid)
}

func makePrivateDirectoryForSID(path string, user *windows.SID) error {
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is not a regular directory", path)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return &os.PathError{Op: "read security", Path: path, Err: err}
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return &os.PathError{Op: "read owner", Path: path, Err: err}
	}
	if !owner.Equals(user) {
		return fmt.Errorf("directory %s is not owned by the current user", path)
	}
	if hasOwnerOnlyDACL(sd, user) {
		return nil
	}
	// SetEntriesInAcl reads the SID through a bare address, so it is pinned
	// until the call has returned.
	var pinner runtime.Pinner
	pinner.Pin(user)
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
	if err != nil {
		return err
	}
	if err := setDirectoryDACL(h, acl); err != nil {
		return &os.PathError{Op: "set dacl", Path: path, Err: err}
	}
	return nil
}

// setDirectoryDACL applies the protected DACL through the handle; tests count
// the calls through it.
var setDirectoryDACL = func(h windows.Handle, acl *windows.ACL) error {
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// hasOwnerOnlyDACL reports whether the descriptor is already the one
// makePrivateDirectory sets: protected from inheritance, with a single allow
// entry for user that grants FILE_ALL_ACCESS to the directory and its children.
func hasOwnerOnlyDACL(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) bool {
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return false
	}
	principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE &&
		ace.Header.AceFlags == windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE &&
		ace.Mask == fileAllAccess &&
		principal.Equals(user)
}

// extendedPath returns an absolute path in the form CreateFile accepts beyond
// MAX_PATH, which the os package applies on its own but a raw call does not:
// \\?\ before a drive path, \\?\UNC\ in place of the \\ of a share path. A
// path that already carries a \\?\ or \\.\ prefix, or a relative path, is
// returned unchanged.
func extendedPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	path = filepath.FromSlash(path)
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

// openBlob opens the path itself, never what a reparse point behind it names:
// a symlink, a junction or a directory at the blob's path is refused as
// corrupt. Backup semantics let a junction open at all, so that the attribute
// check, not an access error, is what refuses it.
func openBlob(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("artifact %s is a reparse point: %w", path, ErrCorrupt)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("artifact %s is a directory: %w", path, ErrCorrupt)
	}
	return os.NewFile(uintptr(h), path), nil
}

func syncDirectory(string) error { return nil }

func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.Errno(1295))
}

func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// replaceError tells a blob held open by a reader apart from a real permission
// problem: replacing an open file fails with ERROR_ACCESS_DENIED, so the
// destination is probed for DELETE access, which a sharing violation refuses
// only while another handle is open on it.
func replaceError(err error, dest string) error {
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return err
	}
	name, convErr := windows.UTF16PtrFromString(extendedPath(dest))
	if convErr != nil {
		return err
	}
	h, probeErr := windows.CreateFile(name, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if probeErr == nil {
		windows.CloseHandle(h)
		return err
	}
	if errors.Is(probeErr, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}
