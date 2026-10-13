package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type lockRecord struct {
	PID     int    `json:"pid"`
	Created string `json:"created"`
}

// Lock holds the coordinator's process lock until Release or process exit.
type Lock struct{ file *os.File }

// Acquire takes over an unlocked stale record, or reports the live holder.
func Acquire(cfg Config) (*Lock, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.StateDir, "adad.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		if !isLockContended(err) {
			f.Close()
			return nil, err
		}
		var record lockRecord
		if _, readErr := f.Seek(0, io.SeekStart); readErr == nil {
			_ = json.NewDecoder(f).Decode(&record)
		}
		f.Close()
		return nil, &AlreadyRunningError{PID: record.PID}
	}
	created, err := processCreated(os.Getpid())
	if err != nil {
		_ = unlockFile(f)
		f.Close()
		return nil, fmt.Errorf("ipc: process creation time: %w", err)
	}
	if err := f.Truncate(0); err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = json.NewEncoder(f).Encode(lockRecord{PID: os.Getpid(), Created: created})
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = unlockFile(f)
		f.Close()
		return nil, err
	}
	return &Lock{file: f}, nil
}

// Release gives up the OS lock, leaving the record for stale recovery.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	err := unlockFile(f)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
