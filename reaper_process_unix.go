//go:build darwin || linux

package container

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// The reaper script starts helper processes. Put them in their own group
// so a failed respawn cannot leave an old helper deleting stale entries.
func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// killReaperProcess signals the process group only while the child is
// still owned by this reaper. os.Process.Signal consults the Process
// handle's done state, so a child already reaped by Wait is rejected
// before the numeric group ID can be used.
func killReaperProcess(process *reaperProcess) {
	if process == nil {
		return
	}
	process.killMu.Lock()
	defer process.killMu.Unlock()

	cmd, _, pgid, live := process.identity()
	if !live || cmd == nil || cmd.Process == nil || pgid <= 0 {
		return
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		// The group may have disappeared between the ownership check and
		// the signal. The direct process handle remains safe to use.
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return
		}
	}
}
