//go:build !windows && !darwin && !linux

package container

import "os/exec"

func prepareReaperCommand(_ *exec.Cmd) {}

func killReaperCommand(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
