//go:build !windows && !linux

package executor

// Without Linux /proc, supervision only kills the engine's process group.
// Descendants that create separate groups (including Harness backends) may survive.
func descendantGroups(int) map[int]string { return nil }
func killDescendantGroups(map[int]string) {}
