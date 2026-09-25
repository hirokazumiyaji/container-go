//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package cli

import "os/exec"

// These platforms have no portable process-group or job-object API in this
// package. Cancellation still terminates and reaps the direct CLI child;
// descendants are outside the guarantee and are not reaped here.
func configureProcessTree(*exec.Cmd) {}

type otherProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) {
	return otherProcessTree{}, nil
}

func (otherProcessTree) terminate(cmd *exec.Cmd) error {
	return terminateProcessTree(cmd)
}

func (otherProcessTree) close() {}

func terminateProcessTree(cmd *exec.Cmd) error {
	return directProcessTree{}.terminate(cmd)
}
