//go:build windows

package container

import "os/exec"

func prepareReaperCommand(*exec.Cmd) {}

func killReaperProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
