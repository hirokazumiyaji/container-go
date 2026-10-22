//go:build !windows

package container

import (
	"os/exec"
	"syscall"
)

// configureReaperProcess gives each reaper shell its own process group.
// Backend commands and the timeout helper can then be terminated as a
// unit when the child is stopped or replaced.
func configureReaperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killReaperProcess terminates the shell and every process in its group.
// configureReaperProcess makes the shell a group leader, and the reaper
// script deliberately does not enable job control, so helpers and backend
// commands stay in that group. The caller waits for the shell, which reaps
// it before its PID can be reused; this function must not hold or signal
// numeric descendant PIDs.
func killReaperProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Stop the leader first. A stopped process cannot exit, so its PID (and
	// therefore the PGID, which Setpgid made equal to it) cannot be recycled
	// between this check and the group signal. Signal uses the retained
	// *os.Process, so an already-reaped command reports ErrProcessDone and
	// the numeric group is never touched.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		_ = cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
