//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import "os/exec"

// Unix has no process-group handle that can safely outlive a direct-child
// Wait. The runner therefore guarantees termination and reaping of the
// direct CLI child only. Descendants may continue after the child exits.
func configureProcessTree(*exec.Cmd) {}

type unixProcessTree struct{}

func newProcessTree(*exec.Cmd) (processTree, error) {
	return unixProcessTree{}, nil
}

func (unixProcessTree) terminate(cmd *exec.Cmd) error {
	return terminateProcessTree(cmd)
}

func (unixProcessTree) close() {}

func terminateProcessTree(cmd *exec.Cmd) error {
	return directProcessTree{}.terminate(cmd)
}
