//go:build windows

package container

import "os/exec"

func prepareReaperCommand(_ *exec.Cmd) {}

func killReaperCommand(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
