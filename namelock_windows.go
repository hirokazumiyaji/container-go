package container

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// ErrNameLockCompatibility is kept available on Windows for API and test
// compatibility, even though the platform has no name-addressed reaper path.
var ErrNameLockCompatibility = errors.New("container name-lock namespace is incompatible")

// Apple Container does not exist on Windows and Docker deletes by
// immutable ID, so no cross-process name lock is needed here.
func lockName(context.Context, string) (func(), error) {
	return func() {}, nil
}

// reaperNameLockPaths is deliberately unavailable on Windows. The normal
// reaper registration is already a no-op there; keeping this helper
// fail-closed prevents a future caller from treating a name-addressed entry
// as safe on a platform without the lock protocol.
func reaperNameLockPaths(string) ([]string, error) {
	return nil, errors.New("reaper: name locks are unavailable on windows")
}

func nameLockIdentity(string) (string, error) {
	return "", errors.New("reaper: name lock identity is unavailable on windows")
}

func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
