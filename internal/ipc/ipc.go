// Package ipc provides the owner's private local coordinator channel and singleton lock.
package ipc

import (
	"errors"
	"fmt"
	"regexp"
)

// Config identifies a private coordinator endpoint and its persistent state directory.
type Config struct {
	StateDir string
	Name     string
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c Config) validate() error {
	if c.StateDir == "" || len(c.Name) > 64 || !validName.MatchString(c.Name) {
		return errors.New("ipc: state directory and a simple nonempty name are required")
	}
	return nil
}

// AlreadyRunningError reports the process recorded as holding the coordinator lock.
type AlreadyRunningError struct {
	PID     int
	Created string
}

func (e *AlreadyRunningError) Error() string {
	if e.PID <= 0 || e.Created == "" {
		return "ipc: coordinator already running (holder unknown)"
	}
	return fmt.Sprintf("ipc: coordinator already running as PID %d (created %s)", e.PID, e.Created)
}
