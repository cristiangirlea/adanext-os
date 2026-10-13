//go:build !windows && !linux

package ipc

func processCreated(int) (string, error) { return "unsupported", nil }
