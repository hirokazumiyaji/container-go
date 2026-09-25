//go:build aix

package container

import (
	"errors"
	"syscall"
)

// AIX exposes Setpgid and supports negative-PID kill, but the Go syscall
// package does not expose Getpgid. Probe the group first. ESRCH for the
// negative probe can also mean that this PID is a live non-leader, so use a
// positive liveness probe before reporting that the process is gone.
func reaperProcessGroupID(pid int) (int, error) {
	err := syscall.Kill(-pid, syscall.Signal(0))
	if err == nil {
		return pid, nil
	}
	if !errors.Is(err, syscall.ESRCH) {
		return 0, err
	}
	if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
		return 0, nil
	} else if errors.Is(err, syscall.ESRCH) {
		return 0, err
	} else {
		return 0, err
	}
}
