//go:build !unix

package script

import "os/exec"

// prepare does nothing here: this platform has no process groups to put the
// script in.
func prepare(cmd *exec.Cmd) {}

// kill terminates the script itself. Without process groups there is nothing
// further cronkite can reach, so a child the script started has to look after
// itself; the timeout still stops the run and is still recorded as a failure.
func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
