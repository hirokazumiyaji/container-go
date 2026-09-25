//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func prepareReaperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killReaperCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	// A timed entry can have its own process group. Kill those groups
	// before the reaper's group so a test or an operator terminating the
	// reaper cannot strand a descendant in a nested group.
	descendants := reaperDescendants(cmd.Process.Pid)
	for _, pid := range descendants {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// The process may have exited between the group signal and this
	// fallback. Let the normal Wait path reap it when possible.
	return cmd.Process.Kill()
}

func reaperDescendants(root int) []int {
	var descendants []int
	var visit func(int)
	visit = func(parent int) {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(out)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				continue
			}
			visit(pid)
			descendants = append(descendants, pid)
		}
	}
	visit(root)
	return descendants
}
