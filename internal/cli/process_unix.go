//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessTree gives each CLI invocation its own process group.
// The group is a termination boundary only: signaling it can terminate
// descendants that remain in the group, but it does not reap them. Once
// the direct child is gone, the platform init/subreaper owns descendant
// zombies; descendants that deliberately detach are outside the boundary.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessTree is called while the stream still owns the direct
// child. It never uses the process-group ID after the direct child has been
// waited, because that ID can be reused by an unrelated process.
func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	// os.Process retains its own done state. In particular, signal 0
	// returns os.ErrProcessDone after the direct child has been waited even
	// if the numeric PID has already been reused.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		// EPERM (and unusual platform-specific errors) still mean that a
		// process handle exists; retain the best-effort group signal.
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// A process group can disappear between the liveness check and the
	// signal. The direct child is still owned here, so this fallback does
	// not target a reused PID.
	return cmd.Process.Kill()
}
