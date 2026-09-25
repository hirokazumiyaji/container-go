//go:build openbsd

package container

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// OpenBSD supports WNOWAIT in the kernel, but the syscall package does not
// expose the flag on every supported Go release.
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

func waitForReaperProcessGroupExit(pgid int) {
	if pgid <= 0 {
		return
	}
	for {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killReaperTree(cmd.Process.Pid)
	if pgid > 0 {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	_ = cmd.Process.Kill()
}
