//go:build !windows

package nowindow

import "os/exec"

func set(*exec.Cmd) {}
