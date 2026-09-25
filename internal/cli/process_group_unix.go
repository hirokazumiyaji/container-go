//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	// os.Process retains its own done state. Do not use a numeric PID as
	// proof of liveness after Cmd.Wait has returned, since it may have been
	// reused in the meantime.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		// EPERM still means that the process handle exists; continue with
		// the best-effort group signal below.
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// The group can disappear between the liveness check and the signal.
	// The direct child is still owned here, so this fallback cannot target
	// a reused PID.
	return cmd.Process.Kill()
}
