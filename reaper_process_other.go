//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import "os/exec"

func prepareReaperCommand(*exec.Cmd) {}

func reaperProcessGroupID(*exec.Cmd) int { return 0 }

func waitForReaperTermination(*exec.Cmd) bool { return false }

func waitForReaperProcessGroupExit(int) bool { return true }

func killReaperProcess(cmd *exec.Cmd, _ int) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func finishReaperProcess(*exec.Cmd, int) {
	// The reaper is not enabled on Windows. Do not signal the process after
	// os/exec has reaped it; a saved numeric process identity is not safe to
	// reuse here.
}
