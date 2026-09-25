package container

import (
	"context"
	"fmt"
)

const (
	nameLockMaintenanceFile = ".maintenance.lock"
	nameLockLeaseSuffix     = ".reaper-lease"
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

func reaperNameLockSet(string) ([]string, []string, error) {
	return nil, nil, fmt.Errorf("name-addressed reaper is unavailable on Windows")
}

func reaperNameLockSetForReaper(string) ([]string, []string, []string, error) {
	return nil, nil, nil, fmt.Errorf("name-addressed reaper is unavailable on Windows")
}

func releaseReaperLeaseFiles([]string, []string, []string, bool) error {
	return nil
}

func reaperLeaseExists(string) bool { return false }

func reaperLeasePaths(string) ([]string, error) { return nil, nil }

func reaperLeaseRawName(string) (string, bool, bool) { return "", false, false }

func rawNameLockPath(string) (string, error) {
	return "", fmt.Errorf("name-addressed locks are unavailable on Windows")
}

func rawTransitionalNameLockPath(string) (string, error) {
	return "", fmt.Errorf("name-addressed locks are unavailable on Windows")
}

func rawLegacyNameLockPath(string) (string, error) {
	return "", fmt.Errorf("name-addressed locks are unavailable on Windows")
}

func reaperNameLockPaths(string) (string, string, error) {
	return "", "", fmt.Errorf("name-addressed reaper is unavailable on Windows")
}
