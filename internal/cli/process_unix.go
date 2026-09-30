//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || illumos

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

// processIdentity is an OS-owned reference to the direct child. It remains
// valid across a concurrent cmd.Wait; on Linux, waitid WSTOPPED/WNOWAIT must
// confirm the stopped state before any numeric group signal is attempted.
// Platforms without that proof fail closed to pidfd/direct termination.
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
}

func newProcessTree(cmd *exec.Cmd) (processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, os.ErrProcessDone
	}
	identity, err := openProcessIdentity(cmd.Process.Pid)
	if err != nil {
		return nil, err
	}
	return &unixProcessTree{identity: identity}, nil
}

func (t *unixProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil || t == nil || t.identity == nil {
		return terminateDirectProcess(cmd)
	}
	active, err := t.identity.active()
	if err != nil || !active {
		return terminateProcessIdentity(t.identity, false)
	}
	if err := t.identity.stop(); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		// The stable identity could not stop the child. The pidfd-directed
		// kill remains safe, but cannot provide group-wide active evidence.
		return terminateProcessIdentity(t.identity, false)
	}
	active, err = t.identity.active()
	if err != nil || !active {
		return terminateProcessIdentity(t.identity, false)
	}

	// A numeric group signal is allowed only after waitid confirms the
	// stopped state without reaping it. If that ownership proof is lost,
	// terminate only through the pidfd and never reuse the numeric PGID.
	stopped, err := t.identity.stopped()
	if err != nil || !stopped {
		return terminateProcessIdentity(t.identity, false)
	}
	active, err = t.identity.active()
	if err != nil || !active {
		return terminateProcessIdentity(t.identity, false)
	}

	// The child is stopped, so its current process group cannot change while
	// the lifecycle decision is serialized. Always call the direct handle
	// after a group signal because the child may have escaped its group.
	var groupErr error
	groupSignaled := false
	if pgid, ok := t.identity.groupID(); ok && pgid == cmd.Process.Pid {
		groupErr = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		groupSignaled = groupErr == nil
	}
	killErr := cmd.Process.Kill()
	if killErr == nil {
		return terminationResult{active: true}
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		if groupSignaled {
			return terminationResult{active: true}
		}
		return terminationResult{active: true, err: os.ErrProcessDone}
	}
	if groupErr != nil {
		return terminationResult{active: true, err: errors.Join(groupErr, killErr)}
	}
	return terminationResult{active: true, err: killErr}
}

func terminateProcessIdentity(identity stableProcessIdentity, activeEvidence bool) terminationResult {
	err := identity.kill()
	if err == nil {
		return terminationResult{active: activeEvidence}
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return terminationResult{err: os.ErrProcessDone}
	}
	return terminationResult{err: err}
}

func (t *unixProcessTree) close() {
	if t != nil && t.identity != nil {
		t.identity.close()
	}
}

func terminateDirectProcess(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	// A successful direct handle kill alone does not prove that the child
	// was active rather than already a zombie.
	return terminationResult{}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}

func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	// This compatibility helper may be called after cmd.Wait. Do not open a
	// new identity from a numeric PID at that point; use only the direct
	// handle and make no active/group claim.
	return terminateDirectProcess(cmd)
}
