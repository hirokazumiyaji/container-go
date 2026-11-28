//go:build !windows

package container

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// envFileCleanupOptions describes one cleanup attempt. The zero value is the
// production policy: derive the root from retained ownership, remove only the
// exact children bound to the recorded directory identity, and close the
// writer lock with closeEnvFileLock. Tests override removeAll and closeLock to
// inject failures into that path.
type envFileCleanupOptions struct {
	base      string
	removeAll func(string) error
	closeLock func(*os.File) error
}

// cleanupEnvFile releases the writer lock and removes the complete private
// directory. It is idempotent while ownership state is retained. Removal and
// lock-close failures are returned; callers also retain the directory for a
// deferred retry.
func cleanupEnvFile(dir string) error {
	return cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{})
}

// cleanupEnvFileAt performs one cleanup attempt with the supplied options.
func cleanupEnvFileAt(ctx context.Context, dir string, opts envFileCleanupOptions) error {
	if dir == "" {
		return nil
	}
	base := opts.base
	removeAll := opts.removeAll
	closeLock := opts.closeLock
	if closeLock == nil {
		closeLock = closeEnvFileLock
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()

	// A state transition is part of cleanup, not a best-effort afterthought:
	// once a directory has been renamed to a tombstone, a late root-lock or
	// descriptor error must leave the same state available to the retry.
	if err := lockCleanupEnv(ctx); err != nil {
		return err
	}
	defer cleanupEnvMu.Unlock()

	original := filepath.Clean(dir)
	state := loadEnvCleanupState(original)
	stateLocked := false
	if state == nil {
		state = &activeEnvFile{path: original, original: original}
	} else {
		state.mu.Lock()
		stateLocked = true
		defer state.mu.Unlock()
		if state.path != "" {
			dir = state.path
		}
	}
	wasPending := state.pending

	var lock *os.File
	stateLock := false
	lockAcquired := false
	pathMissing := false
	removeStarted := false
	renamed := false
	cleanupErr := withEnvFileRootContextWithCloseCallback(ctx, base, func(rootCtx context.Context, root string) error {
		if err := rootCtx.Err(); err != nil {
			return err
		}
		if err := validateEnvCleanupPath(root, dir); err != nil {
			return err
		}
		ownedState := state.lock != nil || state.pending || state.dirInfo != nil || state.root != "" || len(state.childInfos) > 0
		currentRootInfo, err := os.Lstat(root)
		if err != nil {
			if ownedState {
				return fmt.Errorf("%w: environment root %q is unavailable: %w", errUnsafeEnvFile, root, err)
			}
			return err
		}
		if ownedState {
			if state.rootInfo == nil {
				return fmt.Errorf("%w: environment root %q has no recorded identity", errUnsafeEnvFile, root)
			}
			if !os.SameFile(state.rootInfo, currentRootInfo) {
				return fmt.Errorf("%w: environment root %q was replaced", errUnsafeEnvFile, root)
			}
		}
		if !ownedState {
			state.rootInfo = currentRootInfo
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			// A pending state whose lock was already released may have
			// completed removal before a late error was reported. Without
			// that durable state, a missing path is uncertain: the old root
			// may still contain the secret under a renamed path.
			if state.pending && state.lock == nil && state.removed && state.dirInfo != nil {
				pathMissing = true
				return nil
			}
			if completed, ok := loadCompletedEnvCleanup(original); ok {
				if completed.root == root && completed.rootInfo != nil && os.SameFile(completed.rootInfo, currentRootInfo) {
					pathMissing = true
					return nil
				}
				return fmt.Errorf("%w: environment root %q no longer matches the completed cleanup", errUnsafeEnvFile, root)
			}
			return fmt.Errorf("%w: environment directory %q is missing without retained pending ownership", errUnsafeEnvFile, dir)
		}
		if err != nil {
			return err
		}
		if err := validatePrivateDirectoryInfo(dir, info); err != nil {
			return err
		}
		if state.root != "" && state.root != root {
			return fmt.Errorf("%w: environment directory %q belongs to another root", errUnsafeEnvFile, dir)
		}
		if ownedState {
			if state.dirInfo == nil {
				return fmt.Errorf("%w: environment directory %q has no recorded identity", errUnsafeEnvFile, dir)
			}
			if !os.SameFile(state.dirInfo, info) {
				return fmt.Errorf("%w: environment directory %q was replaced", errUnsafeEnvFile, dir)
			}
		} else {
			state.dirInfo = info
		}
		state.root = root
		state.path = dir
		if state.original == "" {
			state.original = original
		}

		switch {
		case state.lock != nil:
			lock = state.lock
			stateLock = true
			lockInfo, err := lock.Stat()
			if err != nil {
				return err
			}
			lockPath := filepath.Join(dir, envFileLockName)
			pathInfo, err := os.Lstat(lockPath)
			if err != nil {
				return err
			}
			if err := validatePrivateRegularInfo(lockPath, lockInfo); err != nil {
				return err
			}
			if !os.SameFile(lockInfo, pathInfo) {
				return fmt.Errorf("%w: environment lock %q was replaced", errUnsafeEnvFile, lockPath)
			}
			if !state.lockAcquired {
				locked, lockErr := tryAcquireEnvFileLock(lock)
				if lockErr != nil {
					return lockErr
				}
				if !locked {
					return fmt.Errorf("environment cleanup: directory %q is active", dir)
				}
				state.lockAcquired = true
			}
			lockAcquired = true
		case state.pending:
			// A pending retry may have already removed the lock while
			// removing an earlier child. If it is still present, validate
			// and acquire it before touching any remaining child.
			lock, _, err = openValidatedPrivateFile(filepath.Join(dir, envFileLockName))
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				lock = nil
			} else {
				locked, lockErr := tryAcquireEnvFileLock(lock)
				if lockErr != nil {
					_ = lock.Close()
					lock = nil
					return lockErr
				}
				if !locked {
					_ = lock.Close()
					lock = nil
					return fmt.Errorf("environment cleanup: directory %q is active", dir)
				}
				state.lock = lock
				state.lockAcquired = true
				stateLock = true
				lockAcquired = true
			}
		default:
			return fmt.Errorf("environment cleanup: directory %q has no owned lock", dir)
		}
		if lock != nil && !lockAcquired {
			return fmt.Errorf("environment cleanup: directory %q has no acquired writer lock", dir)
		}

		if err := validateEnvChildIdentities(state, dir, state.pending); err != nil {
			return err
		}
		if !state.pending && !strings.HasPrefix(filepath.Base(dir), envFileTombstonePrefix) {
			marker, _, err := openValidatedEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker, os.O_RDONLY)
			if err != nil {
				return err
			}
			if err := marker.Close(); err != nil {
				return err
			}
		}
		if err := validateExpectedEnvChildren(dir); err != nil {
			return err
		}
		if state.pending || strings.HasPrefix(filepath.Base(dir), envFileTombstonePrefix) {
			if err := validateEnvMarkerIfPresent(dir); err != nil {
				return err
			}
		}

		// Rename while the writer lock is held. A crash after this point
		// leaves a recognizable tombstone rather than a half-marked live
		// directory; a later stale scan can finish the exact-child removal.
		if !strings.HasPrefix(filepath.Base(dir), envFileTombstonePrefix) {
			tombstone, renameErr := renameEnvDirToTombstone(dir, info)
			if tombstone != "" {
				dir = tombstone
				renamed = true
				state.path = dir
				tombstoneInfo, statErr := os.Lstat(dir)
				if statErr != nil {
					return statErr
				}
				if state.dirInfo == nil {
					return fmt.Errorf("%w: environment directory %q has no validated identity after tombstone rename", errUnsafeEnvFile, dir)
				}
				if !os.SameFile(state.dirInfo, tombstoneInfo) {
					return fmt.Errorf("%w: environment directory %q changed during tombstone rename", errUnsafeEnvFile, dir)
				}
			}
			if renameErr != nil {
				return renameErr
			}
		}

		removeStarted = true
		if removeAll == nil {
			if state.dirInfo == nil {
				return fmt.Errorf("%w: environment directory %q has no validated identity for removal", errUnsafeEnvFile, dir)
			}
			if err := removeExpectedEnvChildrenWithChildren(dir, state.dirInfo, state.childInfos, state.pending); err != nil {
				return err
			}
		} else if err := removeAll(dir); err != nil {
			return err
		}
		state.removed = true
		return rootCtx.Err()
	}, closeEnvFileLock)

	if lock != nil {
		// A validation failure before the tombstone hand-off must leave the
		// writer lock held. Otherwise a deferred retry would downgrade an
		// active directory to an apparently lock-less unsafe entry.
		if stateLock && !removeStarted && !renamed && !pathMissing {
			lock = nil // retain state.lock for the next attempt
		} else {
			closeErr := closeLock(lock)
			// Whether release or Close reports an error, the descriptor is
			// no longer safe to reuse. Ownership is represented by the
			// pending state until a later retry confirms the directory is gone.
			if state != nil && stateLock {
				state.lock = nil
				state.lockAcquired = false
			}
			cleanupErr = errors.Join(cleanupErr, closeErr)
			lock = nil
		}
	}

	// Keep the state until both the filesystem operation and all late
	// close/release operations succeeded. This is what prevents a successful
	// removal followed by a late error from orphaning the directory.
	dirExists := false
	if info, err := os.Lstat(dir); err == nil {
		dirExists = true
		if state != nil {
			if state.dirInfo == nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%w: environment directory %q has no validated identity", errUnsafeEnvFile, dir))
			} else if !os.SameFile(state.dirInfo, info) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%w: environment directory %q was replaced during cleanup", errUnsafeEnvFile, dir))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if state != nil {
		state.path = dir
		if cleanupErr == nil && removeStarted && dirExists {
			cleanupErr = fmt.Errorf("environment cleanup: directory %q remains after removal", dir)
		}
		if cleanupErr == nil && !dirExists && state.root != "" && state.rootInfo != nil {
			currentRootInfo, rootStatErr := os.Lstat(state.root)
			if rootStatErr != nil {
				cleanupErr = fmt.Errorf("%w: environment root %q is unavailable during cleanup finalization: %w", errUnsafeEnvFile, state.root, rootStatErr)
			} else if !os.SameFile(state.rootInfo, currentRootInfo) {
				cleanupErr = fmt.Errorf("%w: environment root %q was replaced during cleanup finalization", errUnsafeEnvFile, state.root)
			}
		}
		if cleanupErr == nil && !dirExists {
			recordCompletedEnvCleanup(state)
			if stateLocked {
				clearEnvCleanupStateLocked(state)
			} else {
				clearEnvCleanupState(state)
			}
		} else if state.root != "" {
			switch {
			case removeStarted || renamed:
				state.pending = true
				pendingEnvCleanups.Store(state.original, state)
				if state.path != state.original {
					pendingEnvCleanups.Store(state.path, state)
				}
				deleteEnvCleanupActiveKey(state.original, state)
				if state.path != state.original {
					deleteEnvCleanupActiveKey(state.path, state)
				}
			case wasPending:
				// A pending retry that still cannot make progress stays
				// pending; it must not be downgraded to a live writer.
				pendingEnvCleanups.Store(state.original, state)
				if state.path != state.original {
					pendingEnvCleanups.Store(state.path, state)
				}
				deleteEnvCleanupActiveKey(state.original, state)
				if state.path != state.original {
					deleteEnvCleanupActiveKey(state.path, state)
				}
			default:
				// Validation failed before the hand-off. Keep the live
				// writer state intact rather than weakening marker checks on
				// a later retry.
				activeEnvFiles.Store(state.original, state)
				deleteEnvCleanupPendingKey(state.original, state)
			}
		}
	}
	return cleanupErr
}

func cleanupEnvFileWithRetry(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), envCleanupDeferredTimeout)
	defer cancel()
	return retryEnvFileCleanupAt(ctx, dir, envFileCleanupOptions{})
}

func cleanupEnvFileAfterUseContext(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), envCleanupDeferredTimeout)
	defer cancel()
	return retryEnvFileCleanupAt(cleanupCtx, dir, envFileCleanupOptions{})
}

// retryEnvFileCleanupAt retries cleanupEnvFileAt within ctx. Cleanup is a
// safety obligation, not ordinary caller work. Keep the caller's
// cancellation/deadline for the foreground attempt, while the deferred or
// background caller supplies a detached bounded context.
func retryEnvFileCleanupAt(ctx context.Context, dir string, opts envFileCleanupOptions) error {
	if dir == "" {
		return nil
	}
	if hook := envCleanupBeforeAttemptHook; hook != nil {
		if err := hook(dir); err != nil {
			return err
		}
	}
	retryCtx, cancel := context.WithTimeout(ctx, envCleanupBackgroundTimeout)
	defer cancel()
	var errs []error
	attempts := envCleanupRetryAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		err := cleanupEnvFileAt(retryCtx, dir, opts)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		if attempt+1 < attempts {
			timer := time.NewTimer(envCleanupRetryInterval)
			select {
			case <-retryCtx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				errs = append(errs, retryCtx.Err())
				return errors.Join(errs...)
			case <-timer.C:
			}
		}
	}
	return errors.Join(errs...)
}

func retryEnvFileCleanupWithError(dir *string) error {
	if dir == nil || *dir == "" {
		return nil
	}
	value := *dir
	base := ""
	if state := loadEnvCleanupState(value); state != nil {
		state.mu.Lock()
		if state.root != "" {
			base = filepath.Dir(state.root)
		}
		state.mu.Unlock()
	}
	retryCtx, cancel := context.WithTimeout(context.Background(), envCleanupDeferredTimeout)
	err := retryEnvFileCleanupAt(retryCtx, value, envFileCleanupOptions{base: base})
	cancel()
	if err == nil {
		*dir = ""
		return nil
	} else {
		log.Printf("container-go: retry environment file cleanup: %v", err)
		// A safety-validation failure is intentionally left pending for
		// an explicit/future stale scan; repeatedly retrying it in the
		// background would race a caller that is repairing the entry.
		if !errors.Is(err, errUnsafeEnvFile) && !errors.Is(err, os.ErrNotExist) {
			scheduleEnvCleanupRetry(value)
		}
		return err
	}
}

func scheduleEnvCleanupRetry(dir string) {
	if dir == "" {
		return
	}
	state := loadEnvCleanupState(dir)
	if state == nil {
		return
	}
	state.mu.Lock()
	key, current, root := state.original, state.path, state.root
	state.mu.Unlock()
	if key == "" {
		key = dir
	}
	if current == "" {
		current = dir
	}
	if _, err := os.Lstat(current); errors.Is(err, os.ErrNotExist) {
		return
	}
	base := ""
	if root != "" {
		base = filepath.Dir(root)
	}
	if _, loaded := pendingEnvCleanupRetries.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer pendingEnvCleanupRetries.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), envCleanupBackgroundTimeout)
		defer cancel()
		attempts := envCleanupRetryAttempts
		if attempts < 1 {
			attempts = 1
		}
		for attempt := 0; attempt < attempts; attempt++ {
			if loadEnvCleanupState(dir) == nil {
				return
			}
			if _, err := os.Lstat(current); errors.Is(err, os.ErrNotExist) {
				return
			}
			err := cleanupEnvFileAt(ctx, dir, envFileCleanupOptions{base: base})
			if err == nil {
				return
			}
			if attempt+1 == attempts {
				break
			}
			timer := time.NewTimer(envCleanupRetryInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
		if state := loadEnvCleanupState(dir); state != nil {
			log.Printf("container-go: environment file cleanup remains pending for %q", dir)
		}
	}()
}

type pendingEnvCleanup struct {
	dir  string
	base string
}

type pendingEnvCleanupError struct {
	err error
}

func (e *pendingEnvCleanupError) Error() string { return e.err.Error() }
func (e *pendingEnvCleanupError) Unwrap() error { return e.err }

func isPendingEnvCleanupError(err error) bool {
	var pending *pendingEnvCleanupError
	return errors.As(err, &pending)
}

func isRetryablePendingEnvCleanupError(err error) bool {
	if !isPendingEnvCleanupError(err) {
		return false
	}
	return !errors.Is(err, errUnsafeEnvFile)
}

func snapshotPendingEnvCleanupsLocked() []pendingEnvCleanup {
	seen := map[*activeEnvFile]struct{}{}
	var pending []pendingEnvCleanup
	pendingEnvCleanups.Range(func(_, value any) bool {
		state, ok := value.(*activeEnvFile)
		if !ok || state == nil {
			return true
		}
		if _, ok := seen[state]; ok {
			return true
		}
		seen[state] = struct{}{}
		state.mu.Lock()
		dir, root := state.original, state.root
		state.mu.Unlock()
		if dir == "" {
			return true
		}
		base := ""
		if root != "" {
			base = filepath.Dir(root)
		}
		pending = append(pending, pendingEnvCleanup{dir: dir, base: base})
		return true
	})
	return pending
}

func drainPendingEnvCleanupsContext(ctx context.Context, pending []pendingEnvCleanup) error {
	if len(pending) == 0 {
		return nil
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	var errs []error
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		err := cleanupEnvFileAt(ctx, item.dir, envFileCleanupOptions{base: item.base})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &pendingEnvCleanupError{err: errors.Join(errs...)}
}

func loadEnvCleanupState(dir string) *activeEnvFile {
	if value, ok := activeEnvFiles.Load(dir); ok {
		if state, ok := value.(*activeEnvFile); ok && state != nil {
			return state
		}
	}
	if value, ok := pendingEnvCleanups.Load(dir); ok {
		if state, ok := value.(*activeEnvFile); ok && state != nil {
			return state
		}
	}
	return nil
}

func deleteEnvCleanupActiveKey(key string, state *activeEnvFile) {
	if key == "" || state == nil {
		return
	}
	activeEnvFiles.CompareAndDelete(key, state)
}

func deleteEnvCleanupPendingKey(key string, state *activeEnvFile) {
	if key == "" || state == nil {
		return
	}
	pendingEnvCleanups.CompareAndDelete(key, state)
}

func deleteEnvCleanupStateKey(key string, state *activeEnvFile) {
	deleteEnvCleanupActiveKey(key, state)
	deleteEnvCleanupPendingKey(key, state)
}

func clearEnvCleanupState(state *activeEnvFile) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	clearEnvCleanupStateLocked(state)
}

func clearEnvCleanupStateLocked(state *activeEnvFile) {
	if state == nil {
		return
	}
	deleteEnvCleanupStateKey(state.original, state)
	deleteEnvCleanupStateKey(state.path, state)
	deleteEnvCleanupStateKey(state.staging, state)
	closeEnvChildFilesLocked(state)
}

func closeEnvChildFilesLocked(state *activeEnvFile) {
	for name, f := range state.childFiles {
		if f != nil {
			_ = f.Close()
		}
		delete(state.childFiles, name)
	}
	state.childFiles = nil
}

func validateEnvCleanupPath(root, dir string) error {
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("%w: cleanup path %q contains '..'", errUnsafeEnvFile, dir)
		}
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) != root {
		return fmt.Errorf("%w: cleanup path %q is outside root %q", errUnsafeEnvFile, dir, root)
	}
	name := filepath.Base(clean)
	if name == "." || name == ".." ||
		(!strings.HasPrefix(name, envFileDirPrefix) &&
			!strings.HasPrefix(name, envFileStagingPrefix) &&
			!strings.HasPrefix(name, envFileTombstonePrefix)) {
		return fmt.Errorf("%w: cleanup path %q is not a library directory", errUnsafeEnvFile, dir)
	}
	return nil
}

func renameEnvDirToTombstone(dir string, dirInfo os.FileInfo) (string, error) {
	parent := filepath.Dir(dir)
	for attempts := 0; attempts < 8; attempts++ {
		candidate, err := os.MkdirTemp(parent, envFileTombstonePrefix)
		if err != nil {
			return "", err
		}
		if err := os.Remove(candidate); err != nil {
			return "", err
		}
		if err := os.Rename(dir, candidate); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		// The rename is the ownership hand-off. Verify the destination
		// still names the directory we validated before reporting success.
		info, err := os.Lstat(candidate)
		if err != nil {
			return candidate, err
		}
		if !os.SameFile(dirInfo, info) {
			return candidate, fmt.Errorf("%w: environment directory %q changed during tombstone rename", errUnsafeEnvFile, dir)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w: could not allocate environment cleanup tombstone", errUnsafeEnvFile)
}
