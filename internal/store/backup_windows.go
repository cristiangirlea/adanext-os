package store

import "golang.org/x/sys/windows"

func publishBackupNoReplace(tmpPath, destPath string) error {
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, 0)
}

// Windows syncs the backup file; directory fsync is a Unix operation.
func syncBackupDirectory(string) error { return nil }
