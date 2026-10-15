package store

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// publishBackupNoReplace moves the backup into place without replacing an
// existing file. MOVEFILE_WRITE_THROUGH makes the rename durable before the
// call returns, so a power cut cannot lose the name.
func publishBackupNoReplace(tmpPath, destPath string) error {
	from, err := windows.UTF16PtrFromString(extendedPath(tmpPath))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(extendedPath(destPath))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

// extendedPath adds the \\?\ prefix that the os package adds to absolute paths
// of 248 characters or more and that MoveFileEx, called directly, lacks:
// without it a destination over MAX_PATH passes the earlier checks and fails
// to publish. Shorter paths, device paths and paths already prefixed are
// returned as they are.
func extendedPath(path string) string {
	if len(path) < 248 {
		return path
	}
	path = filepath.Clean(path)
	switch {
	case strings.HasPrefix(path, `\\?\`), strings.HasPrefix(path, `\??\`), strings.HasPrefix(path, `\\.\`):
		return path
	case strings.HasPrefix(path, `\\`):
		return `\\?\UNC` + path[1:]
	default:
		return `\\?\` + path
	}
}

// Windows syncs the backup file; directory fsync is a Unix operation.
func syncBackupDirectory(string) error { return nil }
