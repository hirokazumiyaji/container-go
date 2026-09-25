package container

import "context"

// Apple Container does not exist on Windows and Docker deletes by
// immutable ID, so no cross-process name lock is needed here.
func lockName(context.Context, string) (func(), error) {
	return func() {}, nil
}

// The watchdog reaper is disabled on Windows, so no barrier is needed.
// The reaper protocol is fail-closed, so reporting no barrier makes the
// child skip name-addressed entries instead of deleting unguarded.
func reaperNameLockMetadata(string) ([]string, []string, error) { return nil, nil, nil }
