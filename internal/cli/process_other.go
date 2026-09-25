//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package cli

import (
	"os"
	"os/exec"
)

// These platforms have no portable process-group implementation in the
// standard library. Cancellation still terminates and reaps the direct CLI
// child, but detached descendants cannot be guaranteed.
func configureProcessTree(*exec.Cmd) {}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
