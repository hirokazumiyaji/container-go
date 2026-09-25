//go:build solaris

package container

import (
	"errors"
	"syscall"
)

// Solaris exposes Setpgid and negative-PID kill, but its Go syscall
// package does not expose Getpgid. Probe the group before signaling it;
// a missing group is handled as a direct-process kill by the caller.
func reaperProcessGroupID(pid int) (int, error) {
	err := syscall.Kill(-pid, syscall.Signal(0))
	if err == nil {
		return pid, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return 0, nil
	}
	return 0, err
}
