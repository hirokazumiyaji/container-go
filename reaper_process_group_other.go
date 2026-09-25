//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package container

import (
	"fmt"
	"os"
	"os/exec"
)

func trustedReaperTool(name string) (string, error) {
	return "", fmt.Errorf("reaper: trusted %s is unavailable on this platform", name)
}

type reaperGroupOwner struct{ pid int }

func newReaperGroupOwner() (*reaperGroupOwner, error)   { return nil, nil }
func prepareReaperCommand(*exec.Cmd, *reaperGroupOwner) {}
func (g *reaperGroupOwner) attach(int, any, any)        {}
func (g *reaperGroupOwner) setSentinelPID(int)          {}
func (g *reaperGroupOwner) signal() bool                { return false }
func (g *reaperGroupOwner) finish()                     {}
func waitForReaperSentinelReady(*os.File) (int, error)  { return 0, nil }
func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}
func terminateReaperProcess(process *reaperProcess) {
	if process != nil && process.cmd != nil && process.cmd.Process != nil {
		_ = process.cmd.Process.Kill()
	}
}
func finishReaperProcess(process *reaperProcess) {
	if process != nil {
		removeReaperStatusDir(process.statusDir)
	}
}
func waitForReaperGroupExit(int) bool         { return true }
func cleanupReaperDescendants(*reaperProcess) {}
