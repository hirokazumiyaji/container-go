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

func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		return 0
	}
	return pgid
}

// killReaperProcess terminates the shell and every process in its group.
// configureReaperProcess makes the shell a group leader and the reaper runs
// non-interactively, so children cannot move to another group. The caller
// waits for the shell, which reaps it before its PID can be reused; this
// function must not hold or signal numeric descendant PIDs.
func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid > 0 && pgid == cmd.Process.Pid {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}
