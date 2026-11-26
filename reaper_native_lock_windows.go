//go:build windows

package container

import (
	"context"
	"time"
)

// Windows has no name-lock namespace: reaperNameLockPath returns an empty
// path, and a Docker name-addressed entry is reclaimed through the immutable
// ID its inspect reports instead of a locked name delete. An entry that
// still carries a lock path cannot be verified there and is skipped rather
// than deleted without the shared lock.
func lockReaperName(context.Context, string, time.Duration) (func(), error) {
	return nil, errReaperLockSkipped
}
