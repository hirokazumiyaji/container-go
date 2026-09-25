//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
)

// configureProcessTree is intentionally a no-op on Windows. The standard
// library does not expose a Job Object handle, so termination uses the
// platform's taskkill tree operation below instead. The PID is numeric and
// passed as a fixed argv, never through a shell. Detached/reparented
// descendants are outside taskkill's best-effort guarantee, and this
// package does not reap descendants.
func configureProcessTree(*exec.Cmd) {}

// terminateProcessTree is called only while the stream owns the direct
// child. The caller must not invoke it after cmd.Wait has returned: the
// numeric PID can then be reused by an unrelated process.
func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	taskErr := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
	killErr := cmd.Process.Kill()
	if taskErr == nil {
		return nil
	}
	if killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
		// The direct process is gone; taskkill can race with normal exit.
		return nil
	}
	return taskErr
}
