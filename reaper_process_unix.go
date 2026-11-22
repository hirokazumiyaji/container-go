//go:build !windows

package container

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// configureReaperProcess gives each reaper shell its own process group.
// Backend commands and the timeout helper can then be terminated as a
// unit when the child is stopped or replaced.
func configureReaperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// reaperJob carries no Unix state: the process group set up before Start is
// the whole teardown, and killReaperProcess signals it. The type exists so
// the reaper's stop path is written once for every platform.
type reaperJob struct{}

// attachReaperJob is a no-op on Unix; the group is configured in
// configureReaperProcess, before the child exists.
func attachReaperJob(*exec.Cmd) reaperJob { return reaperJob{} }

// terminate is a no-op on Unix: killReaperProcess owns the group signal.
func (j *reaperJob) terminate() {}

func (j *reaperJob) close() {}

func shellReaperAvailable() bool { return true }

// killReaperProcess terminates the shell and every process in its group.
// configureReaperProcess makes the shell a group leader, and the reaper
// script deliberately does not enable job control, so helpers and backend
// commands stay in that group. The caller waits for the shell, which reaps
// it before its PID can be reused; this function must not hold or signal
// numeric descendant PIDs.
func killReaperProcess(cmd *exec.Cmd) {
	killReaperProcessWithObserver(cmd, cli.ObserveProcessStopped)
}

// killReaperProcessWithObserver is the test seam for the stopped-state
// barrier. SIGSTOP is asynchronous: a group signal sent before the leader
// has actually stopped can let an in-flight backend command fork a
// descendant that the numeric PGID teardown then misses. The group signal
// is therefore issued only after the stop has been observed, and the
// direct handle is the fail-closed fallback when it cannot be.
func killReaperProcessWithObserver(cmd *exec.Cmd, observe func(*os.Process) (bool, error)) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Stop the leader first. A stopped process cannot exit, so its PID (and
	// therefore the PGID, which Setpgid made equal to it) cannot be recycled
	// between the observation and the group signal. Signal uses the retained
	// *os.Process, so an already-reaped command reports ErrProcessDone and
	// the numeric group is never touched.
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		_ = cmd.Process.Kill()
		return
	}
	if stopped, err := observe(cmd.Process); err != nil || !stopped {
		_ = cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
