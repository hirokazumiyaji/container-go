//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// configureProcessTree gives each CLI invocation its own process group.
// The group is a termination boundary only: signaling it can terminate
// descendants that remain in the group, but it does not reap them.
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

func terminateDirectProcessResult(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	return terminationResult{active: true}
}

type unixProcessOps struct {
	signal    func(*os.Process, os.Signal) error
	getpgid   func(int) (int, error)
	killGroup func(int, syscall.Signal) error
	kill      func() error
}

func defaultUnixProcessOps(cmd *exec.Cmd) unixProcessOps {
	return unixProcessOps{
		signal:    (*os.Process).Signal,
		getpgid:   unix.Getpgid,
		killGroup: unix.Kill,
		kill:      cmd.Process.Kill,
	}
}

// terminateProcessTree pins the direct child with SIGSTOP before addressing
// its numeric process group. os.Process serializes Signal with Wait: if Wait
// has already released the child, Signal returns os.ErrProcessDone; if the
// signal succeeds, the child cannot exit and be reaped before the group
// signal. Getpgid then prevents signaling a recycled group if the child
// changed groups after Start.
func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateProcessTreeWithOps(cmd, defaultUnixProcessOps(cmd))
}

func terminateProcessTreeWithOps(cmd *exec.Cmd, ops unixProcessOps) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := ops.signal(cmd.Process, syscall.SIGSTOP); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		// A failed pin cannot make a numeric group signal safe. The
		// retained direct process handle is still safe to terminate.
		if killErr := ops.kill(); killErr != nil {
			if errors.Is(killErr, os.ErrProcessDone) {
				return terminationResult{err: os.ErrProcessDone}
			}
			return terminationResult{err: errors.Join(err, killErr)}
		}
		return terminationResult{}
	}

	var groupErr error
	groupSignaled := false
	if pgid, err := ops.getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		groupErr = ops.killGroup(-cmd.Process.Pid, syscall.SIGKILL)
		groupSignaled = groupErr == nil
	}

	if errors.Is(groupErr, syscall.ESRCH) {
		groupErr = nil
		groupSignaled = false
	}
	killErr := ops.kill()
	switch {
	case killErr == nil:
		if groupErr != nil {
			return terminationResult{active: true, err: groupErr}
		}
		return terminationResult{active: true}
	case errors.Is(killErr, os.ErrProcessDone):
		if groupSignaled {
			return terminationResult{active: true}
		}
		return terminationResult{err: killErr}
	case groupErr != nil:
		return terminationResult{err: errors.Join(groupErr, killErr)}
	default:
		return terminationResult{err: killErr}
	}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}
