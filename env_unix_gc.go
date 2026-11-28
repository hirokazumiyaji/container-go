//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cleanupStaleEnvFiles reclaims directories left by a process that died
// before deferred cleanup ran. It is intentionally not available on Windows.
// Unix writers hold a non-blocking advisory lock for the whole backend call;
// initialized directories are removed only when that lock is free, while
// old staging directories and transactional tombstones are handled by their
// age and exact-child rules.
func cleanupStaleEnvFiles() error {
	return cleanupStaleEnvFilesContext(context.Background())
}

func cleanupStaleEnvFilesContext(ctx context.Context) error {
	return cleanupStaleEnvFilesAtContext(ctx, "")
}

func cleanupStaleEnvFilesAt(base string) error {
	return cleanupStaleEnvFilesAtContext(context.Background(), base)
}

func cleanupStaleEnvFilesAtContext(ctx context.Context, base string) error {
	if err := ensureEnvFileSecurity(); err != nil {
		return err
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	if err := lockCleanupEnv(ctx); err != nil {
		return err
	}
	pending := snapshotPendingEnvCleanupsLocked()
	scanErr := withEnvFileRootContextWithCallback(ctx, base, func(rootCtx context.Context, root string) error {
		if err := cleanupEnvDirsContext(rootCtx, root); err != nil {
			return err
		}
		markEnvDirGC(root, time.Now())
		return nil
	})
	cleanupEnvMu.Unlock()
	// Pending entries are deliberately drained after releasing the root
	// marker lock. cleanupEnvFile needs that same lock, so doing this inside
	// cleanupEnvDirs would deadlock. The retry path performs the exact-child
	// and identity checks again; it never falls back to RemoveAll.
	var drainErr error
	if scanErr == nil || !errors.Is(scanErr, context.Canceled) && !errors.Is(scanErr, context.DeadlineExceeded) {
		drainErr = drainPendingEnvCleanupsContext(ctx, pending)
	}
	if scanErr != nil {
		return scanErr
	}
	return drainErr
}

// cleanupEnvDirsIfDueContext reclaims abandoned directories at most once per
// envDirGCInterval for root. A failed scan is not recorded, so an entry the
// scan found keeps failing writes until it is removed. An entry created after
// a successful scan is detected by the next due scan or by Run's unconditional
// preflight.
func cleanupEnvDirsIfDueContext(ctx context.Context, root string) error {
	now := time.Now()
	if !envDirGCDue(root, now) {
		return nil
	}
	if err := cleanupEnvDirsContext(ctx, root); err != nil {
		return err
	}
	markEnvDirGC(root, now)
	return nil
}

func cleanupEnvDirsContext(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !strings.HasPrefix(name, envFileDirPrefix) &&
			!strings.HasPrefix(name, envFileStagingPrefix) &&
			!strings.HasPrefix(name, envFileTombstonePrefix) {
			continue
		}
		dir := filepath.Join(root, name)
		if _, active := activeEnvFiles.Load(dir); active {
			continue
		}
		if _, pending := pendingEnvCleanups.Load(dir); pending {
			continue
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: stale environment entry %q is not a directory", errUnsafeEnvFile, dir)
		}
		switch {
		case strings.HasPrefix(name, envFileTombstonePrefix):
			if err := removeStaleTombstoneEnvDir(dir, info); err != nil {
				return err
			}
		case strings.HasPrefix(name, envFileDirPrefix):
			if err := removeStaleEnvDir(dir); err != nil {
				return err
			}
		default:
			if now.Sub(info.ModTime()) < envFileStagingStaleAfter {
				continue
			}
			if err := removeStaleStagingEnvDir(dir, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeStaleEnvDir(dir string) (err error) {
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	marker, _, err := openValidatedEnvMarker(markerPath, envFileDirMarker, os.O_RDONLY)
	if err != nil {
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	return removeStaleLockedEnvDir(dir, dirInfo)
}

// removeStaleLockedEnvDir is the transactional removal phase. The writer
// lock is acquired before the directory is renamed, so a crash leaves a
// tombstone that a later scan can finish without mistaking it for a live
// writer.
func removeStaleLockedEnvDir(dir string, dirInfo os.FileInfo) (err error) {
	lockPath := filepath.Join(dir, envFileLockName)
	lock, lockInfo, err := openValidatedPrivateFile(lockPath)
	if err != nil {
		return err
	}
	locked, err := tryAcquireEnvFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !locked {
		return lock.Close()
	}
	defer func() {
		err = errors.Join(err, closeEnvFileLock(lock))
	}()

	currentDirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	currentLockInfo, err := os.Lstat(lockPath)
	if err != nil {
		return err
	}
	if !os.SameFile(dirInfo, currentDirInfo) || !os.SameFile(lockInfo, currentLockInfo) {
		return fmt.Errorf("%w: stale environment directory %q was replaced during validation", errUnsafeEnvFile, dir)
	}
	expectedChildren, err := validatedExpectedEnvChildrenAt(dir, dirInfo)
	if err != nil {
		return err
	}
	tombstone := dir
	if !strings.HasPrefix(filepath.Base(dir), envFileTombstonePrefix) {
		tombstone, err = renameEnvDirToTombstone(dir, dirInfo)
		if err != nil {
			return err
		}
	}
	return removeExpectedEnvChildrenWithChildren(tombstone, dirInfo, expectedChildren, false)
}

func removeStaleTombstoneEnvDir(dir string, dirInfo os.FileInfo) (err error) {
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	expectedChildren, err := validatedExpectedEnvChildrenAt(dir, dirInfo)
	if err != nil {
		return err
	}
	if err := validateEnvMarkerIfPresent(dir); err != nil {
		return err
	}
	lockPath := filepath.Join(dir, envFileLockName)
	lock, lockInfo, err := openValidatedPrivateFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		current, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !os.SameFile(dirInfo, current) {
			return fmt.Errorf("%w: tombstone %q was replaced", errUnsafeEnvFile, dir)
		}
		return removeExpectedEnvChildrenWithChildren(dir, dirInfo, expectedChildren, false)
	}
	if err != nil {
		return err
	}
	locked, err := tryAcquireEnvFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !locked {
		return lock.Close()
	}
	defer func() {
		err = errors.Join(err, closeEnvFileLock(lock))
	}()
	currentDirInfo, statErr := os.Lstat(dir)
	if statErr != nil {
		return statErr
	}
	currentLockInfo, statErr := os.Lstat(lockPath)
	if statErr != nil {
		return statErr
	}
	if !os.SameFile(dirInfo, currentDirInfo) || !os.SameFile(lockInfo, currentLockInfo) {
		return fmt.Errorf("%w: tombstone %q was replaced during validation", errUnsafeEnvFile, dir)
	}
	return removeExpectedEnvChildrenWithChildren(dir, dirInfo, expectedChildren, false)
}

func removeStaleStagingEnvDir(dir string, dirInfo os.FileInfo) error {
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	expectedChildren, err := validatedExpectedEnvChildrenAt(dir, dirInfo)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	_, markerErr := os.Lstat(markerPath)
	if errors.Is(markerErr, os.ErrNotExist) {
		if len(entries) != 0 {
			return fmt.Errorf("%w: unmarked staging directory %q contains unexpected children", errUnsafeEnvFile, dir)
		}
		return removeExpectedEnvChildrenWithChildren(dir, dirInfo, expectedChildren, false)
	}
	if markerErr != nil {
		return markerErr
	}

	lockPath := filepath.Join(dir, envFileLockName)
	_, lockErr := os.Lstat(lockPath)
	if errors.Is(lockErr, os.ErrNotExist) {
		// A crash between marker creation and lock creation is a known
		// staging state. Only the marker is accepted here; any env file
		// or other child remains a fail-closed manual-review case. A
		// partial marker is repaired in place before the tombstone hand-off.
		if len(entries) != 1 || entries[0].Name() != envFileDirMarkerName {
			return fmt.Errorf("%w: staging directory %q has no lock but contains unexpected state", errUnsafeEnvFile, dir)
		}
		if err := repairEnvMarker(markerPath, envFileDirMarker); err != nil {
			return err
		}
		tombstone, err := renameEnvDirToTombstone(dir, dirInfo)
		if err != nil {
			return err
		}
		return removeExpectedEnvChildrenWithChildren(tombstone, dirInfo, expectedChildren, false)
	}
	if lockErr != nil {
		return lockErr
	}
	marker, _, err := openValidatedEnvMarker(markerPath, envFileDirMarker, os.O_RDONLY)
	if err != nil {
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	// The helper rechecks identities and performs the tombstone hand-off.
	return removeStaleLockedEnvDir(dir, dirInfo)
}

func validatedExpectedEnvChildren(dir string) (map[string]os.FileInfo, error) {
	return validatedExpectedEnvChildrenAt(dir, nil)
}

func validatedExpectedEnvChildrenAt(dir string, expectedDirInfo os.FileInfo) (map[string]os.FileInfo, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if expectedDirInfo != nil && !os.SameFile(expectedDirInfo, info) {
		return nil, fmt.Errorf("%w: environment directory %q was replaced before child validation", errUnsafeEnvFile, dir)
	}
	if err := validatePrivateDirectoryInfo(dir, info); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		envFileName:          true,
		envFileLockName:      true,
		envFileDirMarkerName: true,
	}
	validated := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return nil, fmt.Errorf("%w: environment directory %q contains unexpected child %q", errUnsafeEnvFile, dir, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		childInfo, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if err := validatePrivateRegularInfo(path, childInfo); err != nil {
			return nil, err
		}
		validated[entry.Name()] = childInfo
	}
	return validated, nil
}

func validateExpectedEnvChildren(dir string) error {
	_, err := validatedExpectedEnvChildren(dir)
	return err
}

func removeExpectedEnvChildrenWithChildren(dir string, expectedDirInfo os.FileInfo, expectedChildren map[string]os.FileInfo, allowMissing bool) error {
	// Validate the complete allowlist first. Never use RemoveAll here:
	// a foreign child added after validation must make cleanup fail
	// closed rather than be recursively deleted.
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if expectedDirInfo != nil && !os.SameFile(expectedDirInfo, dirInfo) {
		return fmt.Errorf("%w: environment directory %q was replaced before removal", errUnsafeEnvFile, dir)
	}
	for name, expected := range expectedChildren {
		if err := validateEnvChildIdentity(filepath.Join(dir, name), expected, allowMissing); err != nil {
			return err
		}
	}
	validated, err := validatedExpectedEnvChildrenAt(dir, dirInfo)
	if err != nil {
		return err
	}
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		path := filepath.Join(dir, name)
		if hook := envFileBeforeChildUnlinkHook; hook != nil {
			hook(path)
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		expected, ok := validated[name]
		if !ok {
			if bound, boundOK := expectedChildren[name]; boundOK {
				expected, ok = bound, true
			}
		}
		if !ok {
			return fmt.Errorf("%w: environment child %q appeared after validation", errUnsafeEnvFile, path)
		}
		if err := validatePrivateRegularInfo(path, info); err != nil {
			return err
		}
		if !os.SameFile(expected, info) {
			return fmt.Errorf("%w: environment child %q was replaced during removal", errUnsafeEnvFile, path)
		}
		if err := os.Remove(path); err != nil {
			if expectedChildren != nil && errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: environment child %q disappeared before unlink", errUnsafeEnvFile, path)
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	current, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: environment directory %q disappeared during removal", errUnsafeEnvFile, dir)
		}
		return err
	}
	if !os.SameFile(dirInfo, current) {
		return fmt.Errorf("%w: environment directory %q was replaced during removal", errUnsafeEnvFile, dir)
	}
	if err := os.Remove(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: environment directory %q disappeared during removal", errUnsafeEnvFile, dir)
		}
		return err
	}
	return nil
}
