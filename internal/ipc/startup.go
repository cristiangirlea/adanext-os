package ipc

import (
	"os"
	"path/filepath"
)

// StartupLogName is the start-up log in the state directory.
const StartupLogName = "startup.log"

// OpenStartupLog replaces the state directory's start-up log with a new owner-only
// file; a name already there, a link included, is removed rather than followed.
func OpenStartupLog(stateDir string) (*os.File, error) {
	if err := validateStateDir(stateDir); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, StartupLogName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return createOwnerOnly(path)
}
