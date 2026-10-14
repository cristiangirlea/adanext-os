//go:build !linux && !windows

package executor

import "errors"

func processIdentity(int) (string, error) {
	return "", errors.New("process identity unsupported on this platform")
}
