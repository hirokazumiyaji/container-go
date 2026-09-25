//go:build windows

package cli

import (
	"os"
	"os/exec"
)

// configureProcessTree is intentionally a no-op on Windows. The standard
// library does not expose a Job Object handle, so this package terminates
// the direct child through its retained process handle. Detached or
// reparented descendants are outside that guarantee, and this package does
// not reap descendants.
func configureProcessTree(*exec.Cmd) {}

// terminateProcessTree is called only while the stream owns the direct
// child. Process.Kill uses the retained Windows process handle rather than
// a numeric PID lookup, so a reused PID cannot redirect termination to an
// unrelated process.
func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
