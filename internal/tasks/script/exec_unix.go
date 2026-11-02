//go:build unix

package script

import (
	"os/exec"
	"syscall"
)

// prepare puts the script in a process group of its own, so a timeout can stop
// everything it started with one signal.
//
// It also takes the script out of cronkite's process group: an operator pressing
// Ctrl-C stops cronkite, which cancels the run and kills the script's group
// through kill, instead of half-stopping a chain of pipelines.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// kill terminates the script and every process in its group.
//
// Killing only the direct child would leave the script's own children running
// after the run has declared it stopped, which is the case this exists for: a
// script that started a long copy or a daemon of its own. The fallback covers a
// process that already left the group - signal that one rather than nothing.
func kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}
