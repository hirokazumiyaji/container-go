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

// The normal reaper registration is already a no-op on Windows. Keep the
// name-lock helper fail-closed so a future caller cannot accidentally use
// an unlocked name-addressed reaper entry.
func reaperNameLockPath(string) (string, error) {
	return "", errors.New("reaper: name locks are unavailable on windows")
}

func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
