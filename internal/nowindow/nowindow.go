// Package nowindow starts a console program without a console window. adad
// runs detached on Windows, without a console of its own, so each console
// program it starts would otherwise get a new console: a terminal window per
// call, and a console that can be closed under the program.
package nowindow

import "os/exec"

// Set marks cmd to start without a console window. It is a no-op outside
// Windows, and keeps any creation flags already set.
func Set(cmd *exec.Cmd) { set(cmd) }
