//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessTree gives each CLI invocation its own process group. The
// group is a termination boundary only: signaling it can terminate descendants
// that remain in the group, but it does not reap them. Descendants that detach
// are outside the boundary.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

type unixProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) {
	return unixProcessTree{}, nil
}

func (unixProcessTree) terminate(cmd *exec.Cmd) error {
	return terminateProcessTree(cmd)
}

func (unixProcessTree) close() {}

// terminateProcessTree uses the direct process handle before addressing the
// process group. The lifecycle has exactly one Wait caller, and os.Process
// serializes Signal with that Wait: once Wait has marked the process done,
// Signal returns os.ErrProcessDone instead of sending a signal through a
// potentially reused numeric PID. If Signal succeeds, the child is still an
// unreaped process (or a stopped child), so its process-group ID cannot have
// been recycled before the group signal.
func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		// EPERM and unusual platform errors do not make a numeric group
		// signal safe. Fall back to the direct process handle.
		if killErr := cmd.Process.Kill(); killErr != nil {
			if errors.Is(killErr, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if killErr := cmd.Process.Kill(); killErr == nil {
		// The group may have disappeared or rejected the signal after the
		// direct handle was stopped. The direct handle is still safe.
		return nil
	} else if errors.Is(killErr, os.ErrProcessDone) {
		return os.ErrProcessDone
	} else {
		return err
	}
}
