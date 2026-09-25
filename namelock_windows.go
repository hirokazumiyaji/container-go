package container

import "context"

// Apple Container does not exist on Windows and Docker deletes by
// immutable ID, so no cross-process name lock is needed here.
func lockName(context.Context, string) (func(), error) {
	return func() {}, nil
}

// The watchdog reaper is disabled on Windows, so no lock path is needed.
func reaperNameLockPath(string) (string, error) { return "", nil }
