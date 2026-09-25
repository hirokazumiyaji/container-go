//go:build darwin || linux

package container

import (
	"os/exec"
	"syscall"
)

// The reaper script starts helper processes. Put them in their own group
// so a failed respawn cannot leave an old helper deleting stale entries.
func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killReaperCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
