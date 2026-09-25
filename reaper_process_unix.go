//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// The shell script starts timeout helpers and backend children. Put the
// whole reaper in a private process group so recovery can terminate every
// descendant before a replacement is replayed.
func prepareReaperCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// waitForReaperProcessExit waits for the direct child without reaping it.
// Keeping the zombie waitable lets the caller terminate its process group
// before os/exec reaps the PID, so a recycled process-group ID can never
// be signaled accidentally.
func waitForReaperProcessExit(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	for {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return waited == cmd.Process.Pid && err == nil
	}
}

func waitForReaperTermination(cmd *exec.Cmd) bool {
	return waitForReaperProcessExit(cmd)
}

func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid > 0 {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if err == nil {
			return
		}
		// Fall through to the direct handle. It is still owned by
		// os/exec when this function is called during recovery.
	}
	_ = cmd.Process.Kill()
}

// finishReaperProcess runs after the direct shell has been waited. The
// process group was terminated before Wait while the child was still
// owned; only wait for the group to disappear now. Re-signaling a saved
// numeric ID after Wait could hit an unrelated, recycled process group.
func finishReaperProcess(cmd *exec.Cmd, pgid int) {
	if pgid <= 0 {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pgid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
