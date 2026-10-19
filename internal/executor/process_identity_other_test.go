//go:build !linux && !windows

package executor

import (
	"errors"
	"time"
)

func processIdentity(int) (string, error) {
	return "", errors.New("process identity unsupported on this platform")
}

func waitProcessGone(int, string, time.Duration) error {
	return nil
}
