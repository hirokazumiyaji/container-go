//go:build windows

package container

import "os/exec"

// The watchdog reaper is disabled on Windows, so there is no process
// group to detach, supervise, or terminate. The helpers keep the reaper
// lifecycle code platform-independent.
func prepareReaperCommand(*exec.Cmd) {}

func reaperProcessGroupID(*exec.Cmd) int { return 0 }

func killReaperProcess(cmd *exec.Cmd, _ int) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func finishReaperProcess(int) {}

func terminateReaperProcess(cmd *exec.Cmd, pgid int, _ bool) {
	if cmd == nil {
		return
	}
	killReaperProcess(cmd, pgid)
}
