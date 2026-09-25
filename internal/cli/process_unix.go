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

func (unixProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	return terminateProcessTreeResult(cmd)
}

func (unixProcessTree) close() {}

func terminateDirectProcess(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	// A successful direct handle kill alone does not prove that the child
	// was active rather than already a zombie. The process-group path below
	// supplies the positive SIGSTOP evidence used by lifecycle status.
	return terminationResult{}
}

// terminateProcessTree uses the direct process handle before addressing the
// process group. The lifecycle has exactly one Wait caller, and os.Process
// serializes Signal with that Wait: once Wait has marked the process done,
// Signal returns os.ErrProcessDone instead of sending a signal through a
// potentially reused numeric PID. If Signal succeeds, the child is still an
// unreaped process (or a stopped child). The group signal is sent only when
// its current PGID is still the child's PID, so an escaped child cannot make
// us signal a recycled former group.
func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		// EPERM and unusual platform errors do not make a numeric group
		// signal safe. Fall back to the direct process handle.
		if killErr := cmd.Process.Kill(); killErr != nil {
			if errors.Is(killErr, os.ErrProcessDone) {
				return terminationResult{err: os.ErrProcessDone}
			}
			return terminationResult{err: errors.Join(err, killErr)}
		}
		// The direct kill is safe, but the failed SIGSTOP probe means the
		// lifecycle cannot claim positive active-child evidence.
		return terminationResult{}
	}

	// Always call the direct handle after the group signal. The child may
	// have changed its process group after Start, so do not signal a stale
	// numeric group that could have been recycled. The direct handle is
	// sufficient for an escaped child.
	var groupErr error
	groupSignaled := false
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		groupErr = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		groupSignaled = groupErr == nil
	}
	killErr := cmd.Process.Kill()
	if killErr == nil {
		return terminationResult{active: true}
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		if groupSignaled {
			// The direct child was active when SIGSTOP succeeded and the
			// group signal completed its termination.
			return terminationResult{active: true}
		}
		return terminationResult{active: true, err: os.ErrProcessDone}
	}
	if groupErr != nil {
		return terminationResult{active: true, err: errors.Join(groupErr, killErr)}
	}
	return terminationResult{active: true, err: killErr}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}
