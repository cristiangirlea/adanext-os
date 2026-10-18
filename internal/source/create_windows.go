//go:build windows

package source

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createUnder creates the new file rel under root with content, opening each
// directory on its path relative to its parent's handle with
// FILE_OPEN_REPARSE_POINT and refusing a reparse point anywhere on it, so a
// symlink or junction in the tree cannot redirect the write. Windows keeps no
// executable bit; git records it from the snapshot's mode.
func createUnder(root, rel string, _ bool, content []byte) error {
	dir, err := openReparsePoint(root, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() { windows.CloseHandle(dir) }()
	if err := plainDirectory(dir); err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := ntCreate(dir, part, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_OPEN_IF, windows.FILE_DIRECTORY_FILE)
		if err != nil {
			return &os.PathError{Op: "open", Path: rel, Err: err}
		}
		windows.CloseHandle(dir)
		dir = next
		if err := plainDirectory(dir); err != nil {
			return &os.PathError{Op: "open", Path: rel, Err: err}
		}
	}
	h, err := ntCreate(dir, parts[len(parts)-1], windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return &os.PathError{Op: "create", Path: rel, Err: err}
	}
	f := os.NewFile(uintptr(h), rel)
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ntCreate opens or creates name relative to the directory handle parent,
// opening a reparse point itself rather than its target.
func ntCreate(parent windows.Handle, name string, access, attributes, disposition, options uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access, &oa, &iosb, nil, attributes,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			return windows.InvalidHandle, status.Errno()
		}
		return windows.InvalidHandle, err
	}
	return h, nil
}

// plainDirectory refuses a handle that is a reparse point or not a directory.
func plainDirectory(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errLink
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errNotRegular
	}
	return nil
}
