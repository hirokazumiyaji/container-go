//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import "os/exec"

func prepareReaperCommand(_ *exec.Cmd) {}

func reaperProcessGroupID(_ *exec.Cmd) int { return 0 }

func waitForReaperProcessExit(_ *exec.Cmd) bool { return false }
func waitForReaperTermination(_ *exec.Cmd) bool { return false }
func waitForReaperProcessGroupExit(_ int)       {}

func killReaperProcess(cmd *exec.Cmd, _ int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
