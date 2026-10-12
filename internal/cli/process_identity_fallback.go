//go:build dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package cli

import "os"

// These Unix targets do not expose the pidfd/waitid identity primitive used by
// Linux (or the Darwin waitid bridge). Retain the os.Process and use only its
// direct handle; numeric process-group signaling is deliberately disabled.
func openProcessIdentity(process *os.Process) (stableProcessIdentity, error) {
	if process == nil {
		return nil, os.ErrProcessDone
	}
	return retainedProcessIdentity{process: process}, nil
}

func observeProcessStopped(*os.Process) (bool, error) {
	return false, errProcessStopNotObserved
}
