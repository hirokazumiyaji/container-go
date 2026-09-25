//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package cli

import (
	"os"
	"os/exec"
)

// These platforms have no portable process-group or job-object API in this
// package. Cancellation still terminates and reaps the direct CLI child;
// detached descendants cannot be guaranteed and are not reaped here.
func configureProcessTree(*exec.Cmd) {}

type otherProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) {
	return otherProcessTree{}, nil
}

func (otherProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcess(cmd)
}

func (otherProcessTree) close() {}

func terminateDirectProcess(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	return terminationResult{}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}

func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcess(cmd)
}
