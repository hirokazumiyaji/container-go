//go:build aix

//nolint:unused // retained for platform-specific process-identity tests
package container

import "errors"

// AIX does not expose Getpgid through the Go syscall package. The
// production cleanup path uses the owned process handle and does not
// signal a process group; this probe remains for platform-specific tests.
func reaperProcessGroupID(pid int) (int, error) {
	if pid <= 0 {
		return 0, errors.New("reaper: invalid process-group probe pid")
	}
	return 0, errors.New("reaper: process-group probing is unsupported on aix")
}
