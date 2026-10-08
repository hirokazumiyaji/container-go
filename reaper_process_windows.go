//go:build windows

package container

import "os/exec"

func configureReaperProcess(*exec.Cmd) {}

func reaperProcessGroupID(*exec.Cmd) int { return 0 }

func killReaperProcess(cmd *exec.Cmd, _ int) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
