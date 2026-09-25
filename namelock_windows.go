package container

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// Apple Container does not exist on Windows and Docker deletes by
// immutable ID, so no cross-process name lock is needed here.
func lockName(context.Context, string) (func(), error) {
	return func() {}, nil
}

// reaperNameLockPaths is deliberately unavailable on Windows. The normal
// reaper registration is already a no-op there; keeping this helper
// fail-closed prevents a future caller from accidentally treating a
// name-addressed entry as safe on a platform without the lock protocol.
func reaperNameLockPaths(string) ([]string, error) {
	return nil, errors.New("reaper: name locks are unavailable on windows")
}

func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
