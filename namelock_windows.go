package container

import (
	"context"
	"fmt"
)

// nameLockStateRootOverride is defined on Windows so the shared test
// harness can compile; Windows never uses a name-addressed reaper.
var nameLockStateRootOverride string

// Apple Container does not exist on Windows and Docker deletes by
// immutable ID, so no cross-process name lock is needed here.
func lockName(context.Context, string) (func(), error) {
	return func() {}, nil
}

func nameLockPath(string) (string, error) {
	return "", fmt.Errorf("name-addressed locks are unavailable on Windows")
}

func reaperNameLockPaths(string) (string, string, error) {
	return "", "", fmt.Errorf("name-addressed reaper is unavailable on Windows")
}
