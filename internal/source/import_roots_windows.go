package source

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var importDriveType = windows.GetDriveType
var importSecurityInfo = windows.GetSecurityInfo

func localImportVolume(volume string) bool {
	name, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return false
	}
	switch importDriveType(name) {
	case windows.DRIVE_UNKNOWN, windows.DRIVE_REMOTE, windows.DRIVE_NO_ROOT_DIR:
		return false
	}
	return true
}

func importDirectoryPath(dir *os.File) (string, error) {
	buf := make([]uint16, 256)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(dir.Fd()), &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`), nil
		}
		buf = make([]uint16, n+1)
	}
}

const fileDeleteChild = 0x40

// FILE_ADD_FILE and FILE_ADD_SUBDIRECTORY share the two data-write bits.
const importWrite = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL

func walkImportDirectory(volume string, parts []string, rootDepth int, list, checkPermissions bool, anchor *os.File) (*os.File, error) {
	topDepth := importPermissionTopDepth(volume, parts, rootDepth)
	var err error
	var dir *os.File
	full := volume
	for depth := 0; ; depth++ {
		access := uint32(windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
		check := checkPermissions && depth >= topDepth
		if check {
			access |= windows.READ_CONTROL
		}
		if list && depth == len(parts) {
			access |= windows.FILE_LIST_DIRECTORY
		}
		var next windows.Handle
		if depth == 0 {
			next, err = openReparsePoint(volume, access)
		} else {
			name := parts[depth-1]
			next, err = ntCreate(windows.Handle(dir.Fd()), name, access, windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE)
			full = filepath.Join(full, name)
			dir.Close()
		}
		if err != nil {
			if anchor != nil && depth <= rootDepth {
				return nil, errLink
			}
			return nil, err
		}
		dir = os.NewFile(uintptr(next), full)
		err = plainDirectory(next)
		if err == nil && depth == rootDepth && anchor != nil && !SameDirectory(anchor, dir) {
			err = errLink
		}
		if err == nil && check {
			err = importPermissionCheck(dir, depth < rootDepth, depth == topDepth)
		}
		if err != nil {
			dir.Close()
			return nil, err
		}
		if depth == len(parts) {
			return dir, nil
		}
	}
}

func checkImportPermissions(dir *os.File, ancestor, volumeRoot bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return checkImportDACL(windows.Handle(dir.Fd()), user.User.Sid, ancestor, volumeRoot)
}

func trustedImportSID(sid, user *windows.SID) bool {
	return sid != nil && (sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func checkImportDACL(handle windows.Handle, user *windows.SID, ancestor, volumeRoot bool) error {
	const trustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.OWNER_SECURITY_INFORMATION)
	mask := windows.ACCESS_MASK(fileDeleteChild | windows.GENERIC_ALL | windows.WRITE_DAC | windows.WRITE_OWNER)
	if !volumeRoot {
		mask |= windows.DELETE
	}
	if !ancestor {
		mask = importWrite
	}
	sd, err := importSecurityInfo(handle, windows.SE_FILE_OBJECT, info)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || (!trustedImportSID(owner, user) &&
		!(ancestor && owner != nil && owner.String() == trustedInstaller)) {
		return writableImportRoot()
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return writableImportRoot()
	}
	for i := uint16(0); i < acl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			return err
		}
		inheritOnly := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0
		// Inherit-only rights matter below the grant, but cannot replace an ancestor.
		if ace.Mask&mask == 0 || (ancestor && inheritOnly) {
			continue
		}
		sid := importAllowSID(ace)
		if sid != nil && !trustedImportSID(sid, user) && !(inheritOnly && sid.IsWellKnown(windows.WinCreatorOwnerSid)) {
			return writableImportRoot()
		}
	}
	return nil
}

func importAllowSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	// Object allow ACEs put optional GUIDs before the SID.
	switch ace.Header.AceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE, 0x09: // ACCESS_ALLOWED_CALLBACK_ACE_TYPE
		return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	case 0x04, 0x05, 0x0b: // compound, object and callback object allow ACEs
		offset := uintptr(12)
		if ace.Header.AceType != 0x04 {
			if ace.SidStart&1 != 0 {
				offset += 16
			}
			if ace.SidStart&2 != 0 {
				offset += 16
			}
		}
		return (*windows.SID)(unsafe.Add(unsafe.Pointer(ace), offset))
	default:
		return nil
	}
}
