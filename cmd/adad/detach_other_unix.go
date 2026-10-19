//go:build !windows && !linux

package main

func closeRangeOnExec() bool { return false }
