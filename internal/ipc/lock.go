package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

const lockFileName = "adad.lock"

type lockRecord struct {
	PID     int    `json:"pid"`
	Created string `json:"created"`
}

// afterStateDirChecked is a test hook that runs between the state directory
// check and the lock open; it is nil outside tests.
var afterStateDirChecked func()

// Lock holds the coordinator's process lock until Release or process exit.
type Lock struct {
	mu   sync.Mutex
	file *os.File
	cfg  Config
}

// Acquire takes over an unlocked stale record, or reports the live holder.
func Acquire(cfg Config) (*Lock, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	dir, err := openLockDir(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if afterStateDirChecked != nil {
		afterStateDirChecked()
	}
	f, err := dir.openLock()
	if err != nil {
		return nil, err
	}
	if err := validateLockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		if !isLockContended(err) {
			f.Close()
			return nil, err
		}
		record := readLockRecord(f)
		if locked, retryErr := retryStaleLock(f, record); retryErr != nil {
			f.Close()
			return nil, retryErr
		} else if !locked {
			record = readLockRecord(f)
			f.Close()
			return nil, &AlreadyRunningError{PID: record.PID, Created: record.Created}
		}
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
	return &Lock{file: f, cfg: cfg}, nil
}

func readLockRecord(f *os.File) lockRecord {
	var record lockRecord
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		_ = json.NewDecoder(f).Decode(&record)
	}
	return record
}

// Listen binds the endpoint while the coordinator lock is held.
func (l *Lock) Listen() (net.Listener, error) {
	if l == nil {
		return nil, errors.New("ipc: coordinator lock is not held")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil, errors.New("ipc: coordinator lock is not held")
	}
	key, err := newEndpointToken(l.cfg.StateDir)
	if err != nil {
		return nil, err
	}
	listener, err := listen(l.cfg)
	if err != nil {
		return nil, err
	}
	return authenticateListener(listener, key), nil
}

// Release gives up the OS lock, leaving the record for stale recovery.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
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
