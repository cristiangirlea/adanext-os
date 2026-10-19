//go:build !windows && !linux

package executor

import "syscall"

// Without Linux /proc the tree cannot be scanned: only the engine's own group
// is killed, and only while the engine runs (see the decisions log).
func processCreated(int) string { return "" }
func stopTree(func(int, syscall.Signal) error, int, string) map[int]processRecord {
	return nil
}
func killTree(func(int, syscall.Signal) error, map[int]processRecord) {}
