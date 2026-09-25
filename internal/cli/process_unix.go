//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessTree gives each CLI invocation its own process group.
// Descendants that remain in that group are terminated together with the
// direct child; descendants that deliberately detach remain outside the
// guarantee.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// A process group can disappear between Start and cancellation. Fall
	// back to the direct child so cancellation remains prompt.
	return cmd.Process.Kill()
}
