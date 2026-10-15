//go:build !windows && !linux && !darwin

package store

// renameNoReplace has no atomic no-replace rename on this platform.
func renameNoReplace(string, string) error { return ErrPublishUnsupported }
