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
	"sync"
	"time"
)

var (
	// Unix cleanup retries are cheap unlink attempts; Windows has its own
	// backoff because a sharing violation needs time to clear.
	envCleanupRetryAttempts   = 3
	envCleanupRetryInterval   = time.Millisecond
	envCleanupDeferredTimeout = 250 * time.Millisecond
)

var (
	// activeEnvFiles holds the lock descriptor and identity of each env
	// directory made by this process. The descriptor remains open for the
	// whole backend call, even if the directory is unlinked.
	activeEnvFiles sync.Map // map[string]*activeEnvFile

	// pendingEnvCleanups records a trusted directory whose first removal
	// failed. A later deferred retry may continue after a partial child
	// removal. The same state pointer is retained until removal and lock
	// close both succeed; late release/close errors must not make cleanup
	// ownership disappear.
	pendingEnvCleanups sync.Map // map[string]*activeEnvFile

	// cleanupEnvMu prevents two cleanup callers for the same process from
	// racing the ownership transition between active and pending state.
	cleanupEnvMu sync.Mutex

	// pendingEnvCleanupRetries prevents a failed deferred cleanup from
	// starting an unbounded number of background retry loops for one
	// directory.
	pendingEnvCleanupRetries sync.Map // map[string]struct{}

	// completedEnvCleanups preserves the proof needed to make repeated
	// cleanup calls idempotent after active ownership has been cleared.
	completedEnvCleanups     sync.Map // map[string]completedEnvCleanup
	completedEnvCleanupMu    sync.Mutex
	completedEnvCleanupOrder []string
	completedEnvCleanupKeys  map[string]struct{}
	completedEnvCleanupLimit = 1024

	// Test hook: runs immediately after the final ownership state is
	// published, before post-rename identity checks.
	envFileAfterRenameHook func()

	// Test hook: runs after staging ownership and its initial identity are
	// published, before the first post-publication identity check.
	envFileAfterStagingPublishHook func(string)

	// Test hook: runs before a bounded environment cleanup attempt.
	envCleanupBeforeAttemptHook func(string) error

	// Test hook: runs after creation identities are bound and immediately
	// before the staging directory is renamed for publication.
	envFileBeforePublicationHook func(string)

	// Test hook: runs after the initial child allowlist validation and
	// immediately before a child is unlinked.
	envFileBeforeChildUnlinkHook func(string)
)

type activeEnvFile struct {
	mu           sync.Mutex
	lock         *os.File
	lockAcquired bool
	dirInfo      os.FileInfo
	childInfos   map[string]os.FileInfo
	// childFiles keep the recorded children open so Linux cannot reuse
	// their inodes after an unlink. SameFile alone is (dev, ino); without
	// a held descriptor, Remove+Create can collide and look identical.
	childFiles map[string]*os.File
	rootInfo   os.FileInfo
	root       string
	path       string
	original   string
	staging    string
	removed    bool
	pending    bool
}

type completedEnvCleanup struct {
	root     string
	rootInfo os.FileInfo
}

func recordCompletedEnvCleanup(state *activeEnvFile) {
	if state == nil || state.root == "" || state.rootInfo == nil {
		return
	}
	completed := completedEnvCleanup{root: state.root, rootInfo: state.rootInfo}
	if state.original != "" {
		storeCompletedEnvCleanup(state.original, completed)
	}
	if state.path != "" && state.path != state.original {
		storeCompletedEnvCleanup(state.path, completed)
	}
}

func storeCompletedEnvCleanup(key string, completed completedEnvCleanup) {
	if key == "" {
		return
	}
	completedEnvCleanupMu.Lock()
	defer completedEnvCleanupMu.Unlock()
	if completedEnvCleanupKeys == nil {
		completedEnvCleanupKeys = make(map[string]struct{})
	}
	if _, exists := completedEnvCleanupKeys[key]; exists {
		for i, existing := range completedEnvCleanupOrder {
			if existing == key {
				completedEnvCleanupOrder = append(completedEnvCleanupOrder[:i], completedEnvCleanupOrder[i+1:]...)
				break
			}
		}
	} else {
		completedEnvCleanupKeys[key] = struct{}{}
	}
	completedEnvCleanupOrder = append(completedEnvCleanupOrder, key)
	completedEnvCleanups.Store(key, completed)
	limit := completedEnvCleanupLimit
	if limit < 1 {
		limit = 1
	}
	for len(completedEnvCleanupOrder) > limit {
		oldest := completedEnvCleanupOrder[0]
		completedEnvCleanupOrder = completedEnvCleanupOrder[1:]
		delete(completedEnvCleanupKeys, oldest)
		completedEnvCleanups.Delete(oldest)
	}
}

func loadCompletedEnvCleanup(dir string) (completedEnvCleanup, bool) {
	completedEnvCleanupMu.Lock()
	defer completedEnvCleanupMu.Unlock()
	value, ok := completedEnvCleanups.Load(dir)
	if !ok {
		return completedEnvCleanup{}, false
	}
	completed, ok := value.(completedEnvCleanup)
	return completed, ok
}

// writeEnvFile stores env vars in a 0600 file under a 0700 per-user cache
// directory. It never uses TMPDIR.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	return writeEnvFileContext(context.Background(), env)
}

func writeEnvFileContext(ctx context.Context, env map[string]string) (path, dir string, err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	return writeEnvFileAtContext(ctx, "", env)
}

func writeEnvFileAt(base string, env map[string]string) (path, dir string, err error) {
	return writeEnvFileAtContext(context.Background(), base, env)
}

func writeEnvFileAtContext(ctx context.Context, base string, env map[string]string) (path, dir string, err error) {
	return writeEnvFileAtWithRootContextRunner(ctx, base, env, withEnvFileRootContextWithCallback)
}

type envRootRunner func(string, func(string) error) error
type envRootContextRunner func(context.Context, string, func(context.Context, string) error) error

func writeEnvFileAtWithRoot(base string, env map[string]string, runRoot envRootRunner) (path, dir string, err error) {
	return writeEnvFileAtWithRootContext(context.Background(), base, env, runRoot)
}

func writeEnvFileAtWithRootContext(ctx context.Context, base string, env map[string]string, runRoot envRootRunner) (path, dir string, err error) {
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	return writeEnvFileAtWithRootContextRunner(ctx, base, env,
		func(_ context.Context, base string, fn func(context.Context, string) error) error {
			return runRoot(base, func(root string) error { return fn(ctx, root) })
		})
}

func writeEnvFileAtWithRootContextRunner(ctx context.Context, base string, env map[string]string, runRoot envRootContextRunner) (path, dir string, err error) {
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	if runRoot == nil {
		return "", "", errors.New("environment storage: nil root runner")
	}
	operationCtx := ctx
	err = runRoot(ctx, base, func(rootCtx context.Context, root string) error {
		operationCtx = rootCtx //nolint:fatcontext // records the root context for cleanup; no context is derived here.
		if err := cleanupEnvDirsIfDueContext(rootCtx, root); err != nil {
			return err
		}
		var err error
		path, dir, err = createEnvFileContext(rootCtx, root, env)
		return err
	})
	if err != nil {
		// withEnvFileRoot can report a late root-lock release/close error
		// after createEnvFile has already published ownership. Do not
		// discard that path: retry cleanup here, and return it only when
		// the caller must perform a later retry.
		if dir != "" {
			// The root callback may have returned because its child context
			// was canceled. Cleanup is a separate safety obligation, so
			// detach cancellation while retaining the bounded retry budget.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(operationCtx), envCleanupDeferredTimeout)
			cleanupErr := retryEnvFileCleanupAt(cleanupCtx, dir, envFileCleanupOptions{base: base})
			cancel()
			if cleanupErr != nil {
				return path, dir, errors.Join(err, cleanupErr)
			}
		}
		return "", "", err
	}
	return path, dir, nil
}

func createEnvFile(root string, env map[string]string) (path, dir string, err error) {
	ctx, cancel := boundedEnvContext(context.Background())
	defer cancel()
	return createEnvFileContext(ctx, root, env)
}

func createEnvFileContext(ctx context.Context, root string, env map[string]string) (path, dir string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", err
	}
	staging, err := os.MkdirTemp(root, envFileStagingPrefix)
	if err != nil {
		return "", "", err
	}

	// Publish staging ownership before chmod, marker creation, lock setup,
	// or any other fallible cleanup/validation. A caller that receives an
	// error can therefore retry the exact directory even if the process
	// loses the staging path during finalization.
	state := &activeEnvFile{
		rootInfo: rootInfo,
		root:     root,
		path:     staging,
		original: staging,
		staging:  staging,
		pending:  true,
	}
	activeEnvFiles.Store(staging, state)
	pendingEnvCleanups.Store(staging, state)
	path = filepath.Join(staging, envFileName)
	dir = staging

	stagingInfo, err := os.Lstat(staging)
	if err != nil {
		return path, dir, err
	}
	state.mu.Lock()
	state.dirInfo = stagingInfo
	state.mu.Unlock()
	if hook := envFileAfterStagingPublishHook; hook != nil {
		hook(staging)
	}
	if err := validateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := ctx.Err(); err != nil {
		return path, dir, err
	}
	if err := chmodEnvDirectory(staging); err != nil {
		return path, dir, err
	}
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := writeEnvMarker(filepath.Join(staging, envFileDirMarkerName)); err != nil {
		return path, dir, err
	}
	if err := recordEnvChildInfo(state, envFileDirMarkerName, filepath.Join(staging, envFileDirMarkerName)); err != nil {
		return path, dir, err
	}
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, staging, false); err != nil {
		return path, dir, err
	}

	lockPath := filepath.Join(staging, envFileLockName)
	lock, err := openEnvFileNoFollow(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return path, dir, err
	}
	state.mu.Lock()
	state.lock = lock
	state.mu.Unlock()
	if err := lock.Chmod(envFileMode); err != nil {
		return path, dir, err
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		return path, dir, err
	}
	lockPathInfo, err := os.Lstat(lockPath)
	if err != nil {
		return path, dir, err
	}
	if err := validatePrivateRegularInfo(lockPath, lockInfo); err != nil {
		return path, dir, err
	}
	if err := validatePrivateRegularInfo(lockPath, lockPathInfo); err != nil {
		return path, dir, err
	}
	if !os.SameFile(lockInfo, lockPathInfo) {
		return path, dir, fmt.Errorf("%w: environment lock %q was replaced during creation", errUnsafeEnvFile, lockPath)
	}
	if err := recordEnvChildInfo(state, envFileLockName, lockPath); err != nil {
		return path, dir, err
	}
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, staging, false); err != nil {
		return path, dir, err
	}
	if err := acquireEnvFileLock(lock); err != nil {
		return path, dir, err
	}
	state.mu.Lock()
	state.lockAcquired = true
	state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return path, dir, err
	}

	var b []byte
	for _, k := range sortedKeys(env) {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, env[k]...)
		b = append(b, '\n')
	}
	defer clear(b)

	envPath := filepath.Join(staging, envFileName)
	f, err := openEnvFileNoFollow(envPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, envFileMode)
	if err != nil {
		return path, dir, err
	}
	if err := f.Chmod(envFileMode); err != nil {
		_ = f.Close()
		return path, dir, err
	}
	n, writeErr := f.Write(b)
	envInfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return path, dir, statErr
	}
	closeErr := f.Close()
	if writeErr != nil {
		return path, dir, writeErr
	}
	if n != len(b) {
		return path, dir, io.ErrShortWrite
	}
	if closeErr != nil {
		return path, dir, closeErr
	}
	if err := validatePrivateRegularFile(envPath); err != nil {
		return path, dir, err
	}
	envPathInfo, err := os.Lstat(envPath)
	if err != nil {
		return path, dir, err
	}
	if !os.SameFile(envInfo, envPathInfo) {
		return path, dir, fmt.Errorf("%w: environment file %q was replaced during creation", errUnsafeEnvFile, envPath)
	}
	if err := recordEnvChildInfo(state, envFileName, envPath); err != nil {
		return path, dir, err
	}
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, staging, false); err != nil {
		return path, dir, err
	}
	if err := ctx.Err(); err != nil {
		return path, dir, err
	}

	suffix := strings.TrimPrefix(filepath.Base(staging), envFileStagingPrefix)
	if suffix == "" {
		return path, dir, fmt.Errorf("%w: invalid staging directory name %q", errUnsafeEnvFile, staging)
	}
	finalDir := filepath.Join(root, envFileDirPrefix+suffix)
	if _, err := os.Lstat(finalDir); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return path, dir, err
		}
		return path, dir, fmt.Errorf("%w: environment directory %q already exists", errUnsafeEnvFile, finalDir)
	}
	currentRootInfo, err := os.Lstat(root)
	if err != nil {
		return path, dir, err
	}
	if !os.SameFile(rootInfo, currentRootInfo) {
		return path, dir, fmt.Errorf("%w: environment root %q was replaced before publication", errUnsafeEnvFile, root)
	}
	currentStagingInfo, err := os.Lstat(staging)
	if err != nil {
		return path, dir, err
	}
	state.mu.Lock()
	if state.dirInfo == nil || !os.SameFile(state.dirInfo, currentStagingInfo) {
		state.mu.Unlock()
		return path, dir, fmt.Errorf("%w: staging directory %q was replaced before publication", errUnsafeEnvFile, staging)
	}
	state.mu.Unlock()
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, staging, false); err != nil {
		return path, dir, err
	}
	if hook := envFileBeforePublicationHook; hook != nil {
		hook(staging)
	}
	if err := validatePrivateEnvDirIdentity(staging, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, staging, false); err != nil {
		return path, dir, err
	}
	if err := ctx.Err(); err != nil {
		return path, dir, err
	}
	if err := os.Rename(staging, finalDir); err != nil {
		return path, dir, err
	}

	// Transfer the already-published ownership state to the final path
	// before any post-rename identity check. The old staging key remains
	// mapped until successful finalization so an error cannot orphan it.
	state.mu.Lock()
	state.path = finalDir
	state.original = finalDir
	state.mu.Unlock()
	activeEnvFiles.Store(finalDir, state)
	pendingEnvCleanups.Store(finalDir, state)
	dir = finalDir
	path = filepath.Join(finalDir, envFileName)
	if hook := envFileAfterRenameHook; hook != nil {
		hook()
	}

	currentRootInfo, err = os.Lstat(root)
	if err != nil {
		return path, dir, fmt.Errorf("%w: environment root %q is unavailable after publication: %w", errUnsafeEnvFile, root, err)
	}
	if !os.SameFile(rootInfo, currentRootInfo) {
		return path, dir, fmt.Errorf("%w: environment root %q was replaced after publication", errUnsafeEnvFile, root)
	}
	if err := validatePrivateEnvDirIdentity(finalDir, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, finalDir, false); err != nil {
		return path, dir, err
	}
	finalInfo, err := os.Lstat(finalDir)
	if err != nil {
		return path, dir, fmt.Errorf("%w: environment directory %q is unavailable after publication: %w", errUnsafeEnvFile, finalDir, err)
	}
	state.mu.Lock()
	if state.dirInfo == nil || !os.SameFile(state.dirInfo, finalInfo) {
		state.mu.Unlock()
		return path, dir, fmt.Errorf("%w: environment directory %q changed during publication", errUnsafeEnvFile, finalDir)
	}
	state.mu.Unlock()
	if err := validatePrivateEnvDirIdentity(finalDir, stagingInfo); err != nil {
		return path, dir, err
	}
	if err := validateEnvChildIdentities(state, finalDir, false); err != nil {
		return path, dir, err
	}
	if err := ctx.Err(); err != nil {
		return path, dir, err
	}
	state.mu.Lock()
	state.pending = false
	state.mu.Unlock()
	deleteEnvCleanupStateKey(staging, state)
	deleteEnvCleanupStateKey(finalDir, state)
	activeEnvFiles.Store(finalDir, state)
	return path, dir, nil
}

func writeEnvMarker(path string) (err error) {
	f, err := openEnvFileNoFollow(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(envFileMode); err != nil {
		return err
	}
	if err := writeAll(f, envFileDirMarker); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		// Leave the descriptor owned by the defer so a close error cannot
		// skip the final close attempt and leave the marker path behind.
		return err
	}
	f = nil
	return validatePrivateRegularFile(path)
}

func writeAll(w io.Writer, contents string) error {
	n, err := io.WriteString(w, contents)
	if err != nil {
		return err
	}
	if n != len(contents) {
		return io.ErrShortWrite
	}
	return nil
}

func closeEnvFileLock(lock *os.File) error {
	var errs []error
	if err := releaseEnvFileLock(lock); err != nil {
		errs = append(errs, err)
	}
	if err := lock.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

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
