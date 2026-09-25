//go:build !windows && !darwin && !linux

package container

import "os/exec"

func prepareReaperCommand(_ *exec.Cmd) {}

func reaperProcessGroupID(_ *exec.Cmd) int { return 0 }

func killReaperProcess(process *reaperProcess) {
	if process == nil {
		return
	}
	process.killMu.Lock()
	defer process.killMu.Unlock()
	cmd, _, _, live := process.identity()
	if !live || cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
