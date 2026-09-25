//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// Platforms without the Unix process-group API still stop the direct
// process. Windows additionally uses taskkill's tree operation while the
// direct child is owned; detached descendants remain outside that
// best-effort boundary.
func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if runtime.GOOS == "windows" {
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
	return cmd.Process.Kill()
}
