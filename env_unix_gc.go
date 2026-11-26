//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// envDirGCInterval bounds how often the write path scans the cache root for
// abandoned directories while holding the root lock. A staging directory is
// independent of that scan, so reclaiming on every WithEnv/WithExecEnv call
// only lengthened the critical section. Scans triggered by Run's explicit
// security preflight stay unconditional.
const envDirGCInterval = 30 * time.Second

func lockCleanupEnv(ctx context.Context) error {
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cleanupEnvMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(envRootLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

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

func withEnvFileRoot(base string, fn func(string) error) error {
	if fn == nil {
		return errors.New("environment root: nil callback")
	}
	return withEnvFileRootContext(context.Background(), base, func(_ context.Context, root string) error {
		return fn(root)
	})
}

func withEnvFileRootWithClose(base string, fn func(string) error, closeRoot func(*os.File) error) (err error) {
	if fn == nil {
		return errors.New("environment root: nil callback")
	}
	return withEnvFileRootContextWithClose(context.Background(), base, func(_ context.Context, root string) error {
		return fn(root)
	}, closeRoot)
}

func withEnvFileRootContext(ctx context.Context, base string, fn func(context.Context, string) error) error {
	return withEnvFileRootContextWithClose(ctx, base, fn, closeEnvFileLock)
}

func withEnvFileRootContextWithClose(ctx context.Context, base string, fn func(context.Context, string) error, closeRoot func(*os.File) error) (err error) {
	return withEnvFileRootContextWithCloseCallback(ctx, base, fn, closeRoot)
}

func withEnvFileRootContextWithCallback(ctx context.Context, base string, fn func(context.Context, string) error) error {
	return withEnvFileRootContext(ctx, base, fn)
}

type envRootPathComponent struct {
	path  string
	info  os.FileInfo
	uid   uint32
	uidOK bool
}

type envRootPathIdentity struct {
	root       string
	components []envRootPathComponent
}

func envRootComponentPaths(root string) []string {
	clean := filepath.Clean(root)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume
	if strings.HasPrefix(remainder, string(filepath.Separator)) {
		current += string(filepath.Separator)
		remainder = strings.TrimLeft(remainder, string(filepath.Separator))
	}
	var paths []string
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		paths = append(paths, current)
	}
	return paths
}

func captureEnvRootPathIdentity(root string) (envRootPathIdentity, error) {
	canonical, err := canonicalEnvPath(root)
	if err != nil {
		return envRootPathIdentity{}, fmt.Errorf("%w: canonical environment root %q: %v", errUnsafeEnvFile, root, err)
	}
	if canonical != root {
		return envRootPathIdentity{}, fmt.Errorf("%w: environment root %q was not canonical", errUnsafeEnvFile, root)
	}
	if err := validateEnvPathComponents(root); err != nil {
		return envRootPathIdentity{}, err
	}
	identity := envRootPathIdentity{root: root}
	for _, path := range envRootComponentPaths(root) {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return envRootPathIdentity{}, fmt.Errorf("%w: inspect environment root component %q: %v", errUnsafeEnvFile, path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return envRootPathIdentity{}, fmt.Errorf("%w: environment root component %q is not a real directory", errUnsafeEnvFile, path)
		}
		if !trustedEnvPathComponent(info) {
			return envRootPathIdentity{}, fmt.Errorf("%w: environment root component %q is not trusted", errUnsafeEnvFile, path)
		}
		uid, uidOK := envFileUID(info)
		identity.components = append(identity.components, envRootPathComponent{path: path, info: info, uid: uid, uidOK: uidOK})
	}
	if err := validatePrivateDirectory(root); err != nil {
		return envRootPathIdentity{}, err
	}
	return identity, nil
}

func (identity envRootPathIdentity) validate() error {
	canonical, err := canonicalEnvPath(identity.root)
	if err != nil {
		return fmt.Errorf("%w: canonical environment root %q changed: %v", errUnsafeEnvFile, identity.root, err)
	}
	if canonical != identity.root {
		return fmt.Errorf("%w: environment root %q is no longer canonical", errUnsafeEnvFile, identity.root)
	}
	if err := validateEnvPathComponents(identity.root); err != nil {
		return err
	}
	for _, component := range identity.components {
		info, statErr := os.Lstat(component.path)
		if statErr != nil {
			return fmt.Errorf("%w: environment root component %q changed: %v", errUnsafeEnvFile, component.path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: environment root component %q is no longer a real directory", errUnsafeEnvFile, component.path)
		}
		uid, uidOK := envFileUID(info)
		if !os.SameFile(component.info, info) || info.Mode() != component.info.Mode() || !uidOK || !component.uidOK || uid != component.uid {
			return fmt.Errorf("%w: environment root component %q was replaced or changed mode/owner", errUnsafeEnvFile, component.path)
		}
		if !trustedEnvPathComponent(info) {
			return fmt.Errorf("%w: environment root component %q is no longer trusted", errUnsafeEnvFile, component.path)
		}
	}
	if err := validatePrivateDirectory(identity.root); err != nil {
		return err
	}
	return nil
}

func withEnvFileRootContextWithCloseCallback(ctx context.Context, base string, fn func(context.Context, string) error, closeRoot func(*os.File) error) (err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return err
	}
	if fn == nil || closeRoot == nil {
		return errors.New("environment root: nil callback")
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if base == "" {
		base, err = os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locate per-user environment-file root: %w", err)
		}
	}
	root, err := ensureEnvFileRoot(base)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	rootPathIdentity, err := captureEnvRootPathIdentity(root)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(root, envFileRootMarkerName)
	marker, _, err := openOrCreateRootMarkerContext(ctx, root, markerPath)
	if err != nil {
		return err
	}
	if err := acquireEnvFileRootLockContext(ctx, marker); err != nil {
		return errors.Join(err, closeRoot(marker))
	}
	defer func() {
		err = errors.Join(err, closeRoot(marker))
	}()

	// The marker and root may have been replaced while this caller waited
	// for the advisory lock. Revalidate both identities before any cleanup
	// callback runs. A completed creator is accepted even when it already
	// published a child directory.
	currentRootInfo, statErr := os.Lstat(root)
	if statErr != nil {
		return statErr
	}
	if !os.SameFile(rootInfo, currentRootInfo) {
		return fmt.Errorf("%w: environment root %q was replaced", errUnsafeEnvFile, root)
	}
	if err := rootPathIdentity.validate(); err != nil {
		return err
	}
	if _, err := validateHeldRootMarker(marker, markerPath); err != nil {
		return err
	}
	if err := validatePrivateDirectory(root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	callbackErr := fn(ctx, root)
	currentRootInfo, statErr = os.Lstat(root)
	if statErr != nil {
		postCallbackErr := fmt.Errorf("%w: environment root %q disappeared after callback: %v", errUnsafeEnvFile, root, statErr)
		if callbackErr != nil {
			return errors.Join(callbackErr, postCallbackErr)
		}
		return postCallbackErr
	}
	if !os.SameFile(rootInfo, currentRootInfo) {
		postCallbackErr := fmt.Errorf("%w: environment root %q was replaced after callback", errUnsafeEnvFile, root)
		if callbackErr != nil {
			return errors.Join(callbackErr, postCallbackErr)
		}
		return postCallbackErr
	}
	if identityErr := rootPathIdentity.validate(); identityErr != nil {
		if callbackErr != nil {
			return errors.Join(callbackErr, identityErr)
		}
		return identityErr
	}
	if _, markerErr := validateHeldRootMarker(marker, markerPath); markerErr != nil {
		if callbackErr != nil {
			return errors.Join(callbackErr, markerErr)
		}
		return markerErr
	}
	if callbackErr != nil {
		return callbackErr
	}
	return ctx.Err()
}

func validateHeldRootMarker(f *os.File, path string) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		return nil, err
	}
	if info.Size() != int64(len(envFileRootLockMarker)) {
		return nil, fmt.Errorf("%w: environment root marker %q has invalid length", errUnsafeEnvFile, path)
	}
	data := make([]byte, info.Size())
	if len(data) > 0 {
		n, readErr := f.ReadAt(data, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if n != len(data) || string(data) != envFileRootLockMarker {
			return nil, fmt.Errorf("%w: environment root marker %q has invalid contents", errUnsafeEnvFile, path)
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return nil, fmt.Errorf("%w: environment root marker %q was replaced", errUnsafeEnvFile, path)
	}
	return info, nil
}

func rootContainsOnlyMarker(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != envFileRootMarkerName {
			return false, nil
		}
	}
	return true, nil
}

func openOrCreateRootMarkerContext(ctx context.Context, root, markerPath string) (*os.File, os.FileInfo, error) {
	for attempt := 0; attempt < 64; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		marker, info, err := openValidatedEnvMarker(markerPath, envFileRootLockMarker, os.O_RDWR)
		if err == nil {
			return marker, info, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			// Only an otherwise empty root may acquire a new trust marker.
			empty, readErr := rootContainsOnlyMarker(root)
			if readErr != nil {
				return nil, nil, readErr
			}
			if !empty {
				return nil, nil, fmt.Errorf("%w: environment root marker is missing while root contains entries", errUnsafeEnvFile)
			}
			created, createInfo, createErr := createRootMarkerContext(ctx, markerPath)
			if createErr == nil {
				return created, createInfo, nil
			}
			if !errors.Is(createErr, os.ErrExist) {
				return nil, nil, createErr
			}
			// Another initializer won O_EXCL. Reopen and validate it.
			continue
		}

		// An existing invalid marker is repaired only after acquiring its
		// advisory lock. A live creator holds that lock before it writes,
		// so concurrent initialization waits for the creator instead of
		// removing a marker that is still being initialized.
		f, openErr := openEnvFileNoFollow(markerPath, os.O_RDWR, 0)
		if openErr != nil {
			return nil, nil, fmt.Errorf("%w: environment root marker %q is unsafe: %v", errUnsafeEnvFile, markerPath, err)
		}
		if lockErr := acquireEnvFileRootLockContext(ctx, f); lockErr != nil {
			_ = f.Close()
			return nil, nil, lockErr
		}
		// A creator may have completed while this waiter was blocked. In
		// that case the marker is valid and its child state is legitimate;
		// do not reject it merely because the root is no longer empty.
		if _, validateErr := validateHeldRootMarker(f, markerPath); validateErr == nil {
			info, statErr := f.Stat()
			if statErr != nil {
				_ = closeEnvFileLock(f)
				return nil, nil, statErr
			}
			return f, info, nil
		}
		empty, emptyErr := rootContainsOnlyMarker(root)
		if emptyErr != nil {
			_ = closeEnvFileLock(f)
			return nil, nil, emptyErr
		}
		if !empty {
			_ = closeEnvFileLock(f)
			return nil, nil, fmt.Errorf("%w: environment root marker recovery found root entries", errUnsafeEnvFile)
		}
		repairErr := repairRootMarker(f, markerPath)
		if repairErr != nil {
			_ = closeEnvFileLock(f)
			return nil, nil, repairErr
		}
		info, statErr := f.Stat()
		if statErr != nil {
			_ = closeEnvFileLock(f)
			return nil, nil, statErr
		}
		if _, validateErr := validateHeldRootMarker(f, markerPath); validateErr != nil {
			_ = closeEnvFileLock(f)
			return nil, nil, validateErr
		}
		return f, info, nil
	}
	return nil, nil, fmt.Errorf("%w: root marker creation remained contended", errUnsafeEnvFile)
}

func createRootMarkerContext(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	f, err := openEnvFileNoFollow(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return nil, nil, err
	}
	keep := false
	locked := false
	defer func() {
		if keep {
			return
		}
		// Remove only while this descriptor owns the initialization lock.
		// If context cancellation lost the acquisition race, another
		// initializer may already be repairing the marker; unlinking its
		// path would make that waiter operate on a different inode.
		if locked {
			_ = os.Remove(path)
			_ = releaseEnvFileRootLock(f)
		}
		_ = f.Close()
	}()
	if err := f.Chmod(envFileMode); err != nil {
		return nil, nil, err
	}
	// Acquire the lock before the first write. Other initializers can
	// distinguish a live partial marker from a crash and will wait rather
	// than declaring it unsafe.
	if err := acquireEnvFileRootLockContext(ctx, f); err != nil {
		return nil, nil, err
	}
	locked = true
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := writeAll(f, envFileRootLockMarker); err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	keep = true
	return f, info, nil
}

func repairRootMarker(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		return fmt.Errorf("%w: environment root marker %q is unsafe: %v", errUnsafeEnvFile, path, err)
	}
	if info.Size() > int64(len(envFileRootLockMarker)) {
		return fmt.Errorf("%w: environment root marker %q is too long", errUnsafeEnvFile, path)
	}
	data := make([]byte, info.Size())
	if len(data) > 0 {
		n, readErr := f.ReadAt(data, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != len(data) || !strings.HasPrefix(envFileRootLockMarker, string(data)) {
			return fmt.Errorf("%w: environment root marker %q has unsafe contents", errUnsafeEnvFile, path)
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return fmt.Errorf("%w: environment root marker %q was replaced", errUnsafeEnvFile, path)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := writeAll(f, envFileRootLockMarker); err != nil {
		return err
	}
	return nil
}

func releaseEnvFileRootLock(f *os.File) error {
	return releaseEnvFileLock(f)
}

func ensureEnvFileRoot(base string) (string, error) {
	if err := ensureEnvFileBase(base); err != nil {
		return "", err
	}
	base, err := canonicalEnvFileBase(base)
	if err != nil {
		return "", err
	}
	root := filepath.Join(base, envFileRootName)

	// Never resolve the root itself through a symlink. A pre-existing
	// symlink with the library's name is an unsafe entry, even when its
	// target happens to be owned by this user; all later operations use
	// the canonical, non-symlink path returned below.
	if info, statErr := os.Lstat(root); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: environment root %q is not a private directory", errUnsafeEnvFile, root)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}

	err = os.Mkdir(root, envDirMode)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if created {
		if err := chmodEnvDirectory(root); err != nil {
			return "", err
		}
	}
	canonicalRoot, err := canonicalEnvPath(root)
	if err != nil {
		return "", err
	}
	if canonicalRoot != root {
		return "", fmt.Errorf("%w: environment root %q was not canonical", errUnsafeEnvFile, root)
	}
	if err := validateEnvPathComponents(canonicalRoot); err != nil {
		return "", err
	}
	if err := validatePrivateDirectory(canonicalRoot); err != nil {
		return "", err
	}
	return canonicalRoot, nil
}

// ensureEnvFileBase retains the original validation-only helper contract.
// Filesystem callers use canonicalEnvFileBase so they can retain the resolved
// path for all subsequent operations.
func ensureEnvFileBase(base string) error {
	_, err := canonicalEnvFileBase(base)
	return err
}

// canonicalEnvFileBase canonicalizes the cache base before creating anything.
// The returned path is the only path subsequently used for filesystem
// operations. This closes the validation/use gap for symlinked ancestors:
// EvalSymlinks resolves those ancestors once, while explicit '..' components
// are rejected before any filesystem operation.
func canonicalEnvFileBase(base string) (string, error) {
	canonical, err := canonicalEnvPath(base)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(canonical)
	if err := validateEnvPathComponents(parent); err != nil {
		return "", err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	if !trustedEnvParent(parentInfo) {
		return "", fmt.Errorf("%w: cache parent %q is not private to this user", errUnsafeEnvFile, parent)
	}

	info, statErr := os.Lstat(canonical)
	if errors.Is(statErr, os.ErrNotExist) {
		if err := os.Mkdir(canonical, envDirMode); err == nil {
			if err := chmodEnvDirectory(canonical); err != nil {
				return "", err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, statErr = os.Lstat(canonical)
	}
	if statErr != nil {
		return "", statErr
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: cache directory %q is not a private directory", errUnsafeEnvFile, canonical)
	}
	if err := validateEnvPathComponents(canonical); err != nil {
		return "", err
	}
	uid := currentEnvFileUID()
	owner, ok := envFileUID(info)
	if !ok || owner != uid {
		return "", fmt.Errorf("%w: cache directory %q is not owned by this user", errUnsafeEnvFile, canonical)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%w: cache directory %q is group- or world-writable", errUnsafeEnvFile, canonical)
	}
	return canonical, nil
}

func trustedEnvParent(info os.FileInfo) bool {
	return trustedEnvPathComponent(info)
}

func trustedEnvPathComponent(info os.FileInfo) bool {
	// A non-writable directory is not sufficient on its own: a foreign
	// 0755 directory can still be replaced by its owner between validation
	// and use. Ancestors must be owned by root or this user, including a
	// sticky world-writable directory such as /tmp.
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return false
	}
	owner, ok := envFileUID(info)
	if !ok || (owner != 0 && owner != currentEnvFileUID()) {
		return false
	}
	if mode.Perm()&0o022 == 0 {
		return true
	}
	// A sticky world-writable directory such as /tmp prevents another user
	// from replacing a child they do not own. Group-writable directories
	// without the sticky bit are not trusted.
	return mode.Perm()&0o002 != 0 &&
		(mode&os.ModeSticky != 0 || mode&0o1000 != 0)
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

func envDirGCDue(root string, now time.Time) bool {
	envDirGCMu.Lock()
	defer envDirGCMu.Unlock()
	last, ok := envDirGCLast[root]
	return !ok || now.Sub(last) >= envDirGCInterval
}

func markEnvDirGC(root string, now time.Time) {
	envDirGCMu.Lock()
	envDirGCLast[root] = now
	envDirGCMu.Unlock()
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
