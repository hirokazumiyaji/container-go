//go:build openbsd

package container

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// OpenBSD supports WNOWAIT in the kernel, but older Go releases did not
// expose the constant in package syscall. Keep the value local to the
// platform implementation so this file does not make the other Unix builds
// depend on an OpenBSD-only API.
const openBSDWaitNoWait = 0x10

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

func waitForReaperProcessExit(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	for {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(cmd.Process.Pid, &status, openBSDWaitNoWait, nil)
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
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

func waitForReaperProcessGroupExit(pgid int) bool {
	if pgid <= 0 {
		return true
	}
	for {
		err := syscall.Kill(-pgid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		// Only ESRCH confirms that the group is gone. Keep the barrier
		// closed across transient or unexpected probe errors.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		time.Sleep(10 * time.Millisecond)
	}
}

func finishReaperProcess(*exec.Cmd, int) {}
