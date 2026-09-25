//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// configureProcessTree gives each CLI invocation its own process group.
// The group is a termination boundary only: signaling it can terminate
// descendants that remain in the group, but it does not reap them.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

const processStoppedObservationWindow = 20 * time.Millisecond

// stableProcessIdentity is an ownership-safe reference retained after Start.
// A numeric PGID is never used until the identity has positively observed
// SIGSTOP. Platforms without an identity implementation use the retained
// os.Process directly and fail closed from group signaling.
type stableProcessIdentity interface {
	active() (bool, error)
	stop() error
	stopped() (bool, error)
	kill() error
	groupID() (int, bool)
	close()
}

type unixProcessTree struct {
	identity stableProcessIdentity
	pid      int
}

func newProcessTree(cmd *exec.Cmd) (processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, os.ErrProcessDone
	}
	identity, err := openProcessIdentity(cmd.Process)
	if err != nil || identity == nil {
		// A pidfd (or equivalent) is an enhancement. The retained
		// os.Process is still safer than reacquiring a process by a
		// numeric PID, so use it as the conservative direct-child path.
		identity = retainedProcessIdentity{process: cmd.Process}
	}
	return &unixProcessTree{identity: identity, pid: cmd.Process.Pid}, nil
}

func (t *unixProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	if t == nil || t.identity == nil {
		return terminateDirectProcessResult(cmd)
	}
	return terminateProcessIdentity(t.identity, t.pid)
}

func (t *unixProcessTree) close() {
	if t != nil && t.identity != nil {
		t.identity.close()
	}
}

// retainedProcessIdentity owns the *os.Process returned by Start. It is
// intentionally retained even when no kernel identity (for example a pidfd)
// is available: os.Process serializes Signal with Wait and refuses a released
// process, so no post-Wait numeric reacquisition is needed.
type retainedProcessIdentity struct {
	process *os.Process
}

func (p retainedProcessIdentity) active() (bool, error) {
	if p.process == nil {
		return false, os.ErrProcessDone
	}
	err := p.process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func (p retainedProcessIdentity) stop() error {
	if p.process == nil {
		return os.ErrProcessDone
	}
	return p.process.Signal(syscall.SIGSTOP)
}

func (p retainedProcessIdentity) stopped() (bool, error) {
	if p.process == nil {
		return false, os.ErrProcessDone
	}
	return observeProcessStopped(p.process)
}

func (p retainedProcessIdentity) kill() error {
	if p.process == nil {
		return os.ErrProcessDone
	}
	return p.process.Kill()
}

func (p retainedProcessIdentity) groupID() (int, bool) {
	if p.process == nil {
		return 0, false
	}
	pgid, err := unix.Getpgid(p.process.Pid)
	if err != nil {
		return 0, false
	}
	return pgid, true
}

func (retainedProcessIdentity) close() {}

func terminateProcessIdentity(identity stableProcessIdentity, pid int) terminationResult {
	if identity == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	active, err := identity.active()
	if err != nil {
		return terminateIdentityKill(identity, err)
	}
	if !active {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := identity.stop(); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		// The stop operation failed, so a numeric group signal would be
		// unsafe. The retained identity can still kill the direct child.
		return terminateIdentityKill(identity, err)
	}

	stopped, stopErr := identity.stopped()
	if stopErr != nil || !stopped {
		if stopErr == nil {
			stopErr = errProcessStopNotObserved
		}
		// Never signal a numeric process group without the stopped-state
		// barrier. Fall back to the identity-safe direct kill.
		return terminateIdentityKill(identity, stopErr)
	}
	active, err = identity.active()
	if err != nil {
		return terminateIdentityKill(identity, err)
	}
	if !active {
		return terminateIdentityKill(identity, os.ErrProcessDone)
	}

	var groupErr error
	groupSignaled := false
	if pgid, ok := identity.groupID(); ok && pid > 0 && pgid == pid {
		groupErr = unix.Kill(-pgid, syscall.SIGKILL)
		groupSignaled = groupErr == nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		groupErr = nil
		groupSignaled = false
	}

	killErr := identity.kill()
	switch {
	case killErr == nil:
		return terminationResult{active: true, err: groupErr}
	case errors.Is(killErr, os.ErrProcessDone):
		if groupSignaled {
			return terminationResult{active: true}
		}
		return terminationResult{err: killErr}
	default:
		if groupErr != nil {
			return terminationResult{active: true, err: errors.Join(groupErr, killErr)}
		}
		return terminationResult{active: true, err: killErr}
	}
}

// terminateIdentityKill performs the identity-safe direct kill used whenever
// the numeric process group cannot be addressed. A successful kill still
// reports why the group signal was skipped, and never claims positive
// termination evidence: the caller must keep treating the process result as
// the child's own outcome.
func terminateIdentityKill(identity stableProcessIdentity, priorErr error) terminationResult {
	killErr := identity.kill()
	if killErr == nil {
		return terminationResult{err: priorErr}
	}
	if errors.Is(killErr, os.ErrProcessDone) || errors.Is(killErr, syscall.ESRCH) {
		return terminationResult{err: os.ErrProcessDone}
	}
	if priorErr != nil {
		return terminationResult{err: errors.Join(priorErr, killErr)}
	}
	return terminationResult{err: killErr}
}

func terminateDirectProcessResult(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	return terminationResult{active: true}
}

// unixProcessOps and terminateProcessTreeWithOps are a narrow test seam for
// the old process-group ordering test. Production termination goes through
// unixProcessTree and the retained identity above.
type unixProcessOps struct {
	signal    func(*os.Process, os.Signal) error
	getpgid   func(int) (int, error)
	killGroup func(int, syscall.Signal) error
	kill      func() error
	stopped   func() (bool, error)
}

// terminateProcessTreeWithOps is retained for package tests that exercise
// serialization of a group signal with Wait. It is not used by production
// code, which must use the identity and stopped barrier in unixProcessTree.
func terminateProcessTreeWithOps(cmd *exec.Cmd, ops unixProcessOps) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := ops.signal(cmd.Process, syscall.SIGSTOP); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		if killErr := ops.kill(); killErr != nil {
			if errors.Is(killErr, os.ErrProcessDone) {
				return terminationResult{err: os.ErrProcessDone}
			}
			return terminationResult{err: errors.Join(err, killErr)}
		}
		return terminationResult{}
	}
	if ops.stopped != nil {
		stopped, stoppedErr := ops.stopped()
		if stoppedErr != nil || !stopped {
			if stoppedErr == nil {
				stoppedErr = errProcessStopNotObserved
			}
			return terminateIdentityKillOps(ops, stoppedErr)
		}
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
		return terminationResult{active: true, err: groupErr}
	case errors.Is(killErr, os.ErrProcessDone):
		if groupSignaled {
			return terminationResult{active: true}
		}
		return terminationResult{err: killErr}
	case groupErr != nil:
		return terminationResult{active: true, err: errors.Join(groupErr, killErr)}
	default:
		return terminationResult{active: true, err: killErr}
	}
}

func terminateIdentityKillOps(ops unixProcessOps, priorErr error) terminationResult {
	killErr := ops.kill()
	if killErr == nil {
		return terminationResult{err: priorErr}
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		return terminationResult{err: os.ErrProcessDone}
	}
	return terminationResult{err: errors.Join(priorErr, killErr)}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}

// terminateProcessTreeResult is a compatibility helper for callers that do
// not have a retained processTree. It must never reacquire a process from a
// numeric PID; directProcess handles the only safe post-Wait operation.
func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcessResult(cmd)
}

var errProcessStopNotObserved = errors.New("process stop state was not observed")
