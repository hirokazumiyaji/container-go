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
// valid across a concurrent cmd.Wait, so a group signal cannot be aimed at a
// recycled numeric PGID. Platforms without such a reference fail closed to
// direct-handle termination.
type stableProcessIdentity interface {
	active() (bool, error)
	stop() error
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
		return terminateDirectProcess(cmd)
	}
	if err := t.identity.stop(); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return terminationResult{err: os.ErrProcessDone}
		}
		// The stable identity could not stop the child. The direct handle
		// remains safe, but cannot provide group-wide active evidence.
		return terminateDirectProcess(cmd)
	}
	active, err = t.identity.active()
	if err != nil || !active {
		return terminateDirectProcess(cmd)
	}

	// Always call the direct handle after the group signal. A child that
	// changed process groups gets direct termination without touching its
	// former numeric group.
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
