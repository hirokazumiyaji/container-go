//go:build !windows

package container

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// The reaper runs detached in its own process group. A signal aimed at
// this process's group (a terminal hangup, a group-wide kill) then does
// not remove the watchdog before it observes the pipe EOF that tells it
// to clean up, and the shell's timeout helpers and backend children stay
// inside one group that can be terminated as a unit.
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
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		// Never retain a numeric group ID that was not observed as this
		// child's private group. A direct Process.Kill fallback is safer
		// than signalling a possibly reused group.
		return 0
	}
	return pgid
}

func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid > 0 {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return
		}
		// Fall through to the direct handle.
	}
	_ = cmd.Process.Kill()
}

// finishReaperProcess waits for the detached group to disappear so a
// replacement child never races a lingering descendant for the pipe or a
// name barrier.
func finishReaperProcess(pgid int) {
	if pgid <= 0 {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-pgid, syscall.Signal(0)), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// terminateReaperProcess stops a replaced child and its descendants. A
// child that already exited is not signalled again, because its
// process-group ID may already belong to an unrelated process; the group
// is only waited out, since the timeout helpers can still hold the pipe.
func terminateReaperProcess(cmd *exec.Cmd, pgid int, exited bool) {
	if cmd == nil {
		return
	}
	if !exited {
		killReaperProcess(cmd, pgid)
	}
	finishReaperProcess(pgid)
}
