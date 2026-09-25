//go:build darwin || dragonfly || freebsd || linux || netbsd || solaris

package container

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// The reaper and all of the shell helpers start in a private process group.
// The direct shell remains an unreaped child while recovery decides whether
// to signal that group, which keeps the numeric PGID from being recycled.
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

// waitForReaperProcessExit observes the direct child without reaping it.
// WNOWAIT leaves a waitable zombie (or a still-running child) in place so
// the process-group signal can be issued before Cmd.Wait releases the PID.
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

// waitForReaperProcessGroupExit keeps signaling the owned group until it no
// longer exists. The direct child has not been passed to Cmd.Wait yet, so
// its PID cannot be reused while this loop is running. No liveness probe is
// used between a check and a signal.
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
			// Keep the direct child unreaped while an unusual group error is
			// retried. Returning here would allow Cmd.Wait to release the PID
			// while a descendant could still make the numeric group unsafe.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killReaperProcess is called only while exec.Cmd still owns the direct
// child. It deliberately does not perform a zero-signal liveness probe
// followed by a numeric group kill: the unreaped child pins the group identity
// for the signal.
func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Timed backend supervisors own nested groups. Walk their descendants
	// before signaling the root group so recovery cannot leave an old
	// supervisor running beside its replacement.
	killReaperTree(cmd.Process.Pid)
	if pgid > 0 {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	_ = cmd.Process.Kill()
}
