//go:build !windows

package main

// Electron's detached spawn already creates a separate session on Unix.
func detachFromJob([]string) (bool, error) { return false, nil }
