//go:build !windows

package container

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// configureReaperProcess gives each reaper shell its own process group.
// Backend commands and the timeout helper can then be terminated as a
// unit when the child is stopped or replaced.
func configureReaperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func reaperProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		return 0
	}
	return pgid
}

// killReaperProcess terminates the shell's group and any job-control
// descendants that were placed in separate groups. The shell is waited
// by the caller; the short poll also gives descendants time to disappear
// before a replacement process is started.
func killReaperProcess(cmd *exec.Cmd, pgid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	pids := descendantPIDs(pid)
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	for i := len(pids) - 1; i >= 0; i-- {
		_ = syscall.Kill(pids[i], syscall.SIGKILL)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		alive := false
		for _, descendant := range pids {
			if syscall.Kill(descendant, 0) == nil {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func descendantPIDs(parent int) []int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
		pids = append(pids, descendantPIDs(pid)...)
	}
	return pids
}
