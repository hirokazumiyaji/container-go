//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"os/exec"
	"syscall"
)

// The reaper child is placed in its own process group so a later timeout can
// target that group without ever naming the caller's process group.
func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
