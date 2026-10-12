//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !illumos && !windows

package cli

import (
	"os"
	"os/exec"
)

// These platforms have no portable process-group implementation in this
// package. Cancellation still terminates and reaps the direct CLI child;
// detached descendants cannot be guaranteed.
func configureProcessTree(*exec.Cmd) {}

type otherProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) {
	return otherProcessTree{}, nil
}

func (otherProcessTree) terminate(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcessResult(cmd)
}

func (otherProcessTree) close() {}

func terminateDirectProcessResult(cmd *exec.Cmd) terminationResult {
	if cmd == nil || cmd.Process == nil {
		return terminationResult{err: os.ErrProcessDone}
	}
	if err := cmd.Process.Kill(); err != nil {
		return terminationResult{err: err}
	}
	return terminationResult{active: true}
}

func terminateProcessTreeResult(cmd *exec.Cmd) terminationResult {
	return terminateDirectProcessResult(cmd)
}

func terminateProcessTree(cmd *exec.Cmd) error {
	return terminateProcessTreeResult(cmd).err
}
