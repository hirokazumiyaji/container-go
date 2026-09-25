//go:build darwin || dragonfly || freebsd || linux || netbsd || solaris

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

// killReaperProcess is called only while exec.Cmd still owns the direct
// child. The process-group signal must therefore happen before cmd.Wait;
// a saved numeric group ID is never used after the child has been reaped.
func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid > 0 {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if err == nil {
			return
		}
		// Fall through to the direct handle. It is still owned by os/exec
		// when this function is called during recovery.
	}
	_ = cmd.Process.Kill()
}

// waitForReaperProcessGroupExit is a strict barrier. A replacement is not
// allowed to start until the old group has disappeared, including any
// timeout helper's sleep descendants. The direct shell is intentionally
// still a zombie here, so its process-group ID cannot be recycled while
// this loop observes the group.
func waitForReaperProcessGroupExit(pgid int) bool {
	if pgid <= 0 {
		return true
	}
	for {
		err := syscall.Kill(-pgid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		// The group leader is still waitable, so repeated signals remain
		// ownership-safe even if a descendant is briefly slow to die. An
		// unexpected error is not proof that the group disappeared; keep
		// the barrier closed until an ESRCH observation confirms it.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		time.Sleep(10 * time.Millisecond)
	}
}

// finishReaperProcess is intentionally a no-op. The group barrier has
// already completed before os/exec reaps the direct child; signaling a
// saved PGID here could hit an unrelated, recycled process group.
func finishReaperProcess(*exec.Cmd, int) {}
