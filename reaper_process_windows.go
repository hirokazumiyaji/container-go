//go:build windows

package container

import "os/exec"

func configureReaperProcess(*exec.Cmd) {}

func killReaperProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
