//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import "os/exec"

func prepareReaperCommand(*exec.Cmd) {}

func reaperProcessGroupID(*exec.Cmd) int { return 0 }

func waitForReaperTermination(*exec.Cmd) bool { return false }

func killReaperProcess(cmd *exec.Cmd, _ int) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func finishReaperProcess(cmd *exec.Cmd, _ int) {
	// The reaper is not enabled on Windows. Keep the helper available so
	// cross-platform builds retain the same lifecycle code and tests.
	killReaperProcess(cmd, 0)
}
