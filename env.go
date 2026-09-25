package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	envFileRootName       = "containergo-env-v1"
	envFileRootMarkerName = ".root"
	envFileRootLockMarker = "container-go secure environment-file root v1\n"

	envFileDirPrefix       = "containergo-env-"
	envFileStagingPrefix   = ".containergo-env-new-"
	envFileTombstonePrefix = ".containergo-env-gone-"
	envFileDirMarkerName   = ".container-go-env"
	envFileDirMarker       = "container-go environment directory v1\n"
	envFileLockName        = ".lock"
	envFileName            = "env"
	envFileMode            = 0o600
	envDirMode             = 0o700

	// A process that dies while staging releases the root lock, so an
	// abandoned staging directory is eligible for an age-based retry. Fully
	// initialized directories are protected only by their writer lock; age is
	// deliberately not a liveness signal.
	envFileStagingStaleAfter = 24 * time.Hour
)

var (
	// Root-lock polling and cleanup retries are deliberately bounded. The
	// context supplied by Run/Exec still takes precedence when it expires
	// sooner.
	envRootLockPollInterval     = 10 * time.Millisecond
	envFileSecurityTimeout      = 30 * time.Second
	envCleanupRetryAttempts     = 3
	envCleanupRetryInterval     = time.Millisecond
	envCleanupDeferredTimeout   = 250 * time.Millisecond
	envCleanupBackgroundTimeout = 30 * time.Second
)

var (
	errUnsafeEnvFile = errors.New("environment file storage failed safety validation")

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
)

type activeEnvFile struct {
	mu       sync.Mutex
	lock     *os.File
	dirInfo  os.FileInfo
	rootInfo os.FileInfo
	root     string
	path     string
	original string
	pending  bool
}

// validateEnvMap enforces the format understood by both supported
// backends before any CLI command is built. Env-file lines are UTF-8 and
// line-delimited: keys cannot be empty, comments, whitespace, control
// characters, or contain the '=' delimiter. Values may contain spaces and
// '=' but must be valid UTF-8 and free of control characters or line
// separators (including NUL and line breaks).
func validateEnvMap(env map[string]string) error {
	key, value, err := firstInvalidEnv(env)
	if err == nil {
		return nil
	}
	if value {
		return fmt.Errorf("environment variable %s: value %w", key, err)
	}
	return fmt.Errorf("invalid environment variable name %q: %w", key, err)
}

func firstInvalidEnv(env map[string]string) (key string, value bool, err error) {
	for k, v := range env {
		if err := validateEnvKey(k); err != nil {
			return k, false, err
		}
		if err := validateEnvValue(v); err != nil {
			return k, true, err
		}
	}
	return "", false, nil
}

func validateEnvKey(k string) error {
	if k == "" {
		return fmt.Errorf("must not be empty")
	}
	if !utf8.ValidString(k) {
		return fmt.Errorf("must be valid UTF-8")
	}
	if strings.HasPrefix(k, "#") {
		return fmt.Errorf("must not start with '#'")
	}
	// Docker strips a UTF-8 BOM from the first env-file line. Reject it
	// here rather than allowing a key to be silently renamed or treated
	// as a comment.
	if strings.HasPrefix(k, "\uFEFF") {
		return fmt.Errorf("must not start with a byte-order mark")
	}
	for _, r := range k {
		switch {
		case r == '=':
			return fmt.Errorf("must not contain '='")
		case r == 0:
			return fmt.Errorf("must not contain NUL")
		case r == '\n' || r == '\r':
			return fmt.Errorf("must not contain line breaks")
		case unicode.IsSpace(r):
			return fmt.Errorf("must not contain whitespace")
		case unicode.IsControl(r):
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

func validateEnvValue(v string) error {
	if !utf8.ValidString(v) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range v {
		switch {
		case r == 0:
			return fmt.Errorf("must not contain NUL")
		case r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029':
			return fmt.Errorf("must not contain line breaks")
		case unicode.IsControl(r):
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

// writeEnvFile stores env vars in a 0600 file under a 0700 per-user cache
// directory. It never uses TMPDIR. On Windows it fails with
// ErrEnvFileUnsupported because Go's chmod modes do not provide the claimed
// per-user secrecy there.
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
	return writeEnvFileAtWithRootContext(ctx, base, env, func(base string, fn func(string) error) error {
		return withEnvFileRootContext(ctx, base, fn)
	})
}

type envRootRunner func(string, func(string) error) error

func writeEnvFileAtWithRoot(base string, env map[string]string, runRoot envRootRunner) (path, dir string, err error) {
	return writeEnvFileAtWithRootContext(context.Background(), base, env, runRoot)
}

func writeEnvFileAtWithRootContext(ctx context.Context, base string, env map[string]string, runRoot envRootRunner) (path, dir string, err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	if runRoot == nil {
		return "", "", errors.New("environment storage: nil root runner")
	}
	err = runRoot(base, func(root string) error {
		if err := cleanupEnvDirsContext(ctx, root); err != nil {
			return err
		}
		var err error
		path, dir, err = createEnvFile(root, env)
		return err
	})
	if err != nil {
		// withEnvFileRoot can report a late root-lock release/close error
		// after createEnvFile has already published ownership. Do not
		// discard that path: retry cleanup here, and return it only when
		// the caller must perform a later retry.
		if dir != "" {
			cleanupCtx, cancel := context.WithTimeout(ctx, envCleanupDeferredTimeout)
			cleanupErr := cleanupEnvFileAtWithRetryContext(cleanupCtx, base, dir, removeExpectedEnvChildren)
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
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", err
	}
	staging, err := os.MkdirTemp(root, envFileStagingPrefix)
	if err != nil {
		return "", "", err
	}
	keep := false
	cleanupDir := staging
	var lock *os.File
	defer func() {
		if keep {
			return
		}
		var cleanupErr error
		if lock != nil {
			cleanupErr = closeEnvFileLock(lock)
		}
		// Staging cleanup is also constrained to the exact child
		// allowlist. RemoveAll would turn a same-user race or a corrupted
		// staging entry into an unintended recursive delete.
		if removeErr := removeExpectedEnvChildren(cleanupDir); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
		err = errors.Join(err, cleanupErr)
	}()

	if err := chmodEnvDirectory(staging); err != nil {
		return "", "", err
	}
	if err := validatePrivateDirectory(staging); err != nil {
		return "", "", err
	}
	if err := writeEnvMarker(filepath.Join(staging, envFileDirMarkerName), envFileDirMarker); err != nil {
		return "", "", err
	}

	lockPath := filepath.Join(staging, envFileLockName)
	lock, err = openEnvFileNoFollow(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return "", "", err
	}
	if err := lock.Chmod(envFileMode); err != nil {
		return "", "", err
	}
	if err := validatePrivateRegularFile(lockPath); err != nil {
		return "", "", err
	}
	if err := acquireEnvFileLock(lock); err != nil {
		return "", "", err
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
		return "", "", err
	}
	if err := f.Chmod(envFileMode); err != nil {
		_ = f.Close()
		return "", "", err
	}
	n, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil {
		return "", "", writeErr
	}
	if n != len(b) {
		return "", "", io.ErrShortWrite
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if err := validatePrivateRegularFile(envPath); err != nil {
		return "", "", err
	}

	suffix := strings.TrimPrefix(filepath.Base(staging), envFileStagingPrefix)
	if suffix == "" {
		return "", "", fmt.Errorf("%w: invalid staging directory name %q", errUnsafeEnvFile, staging)
	}
	dir = filepath.Join(root, envFileDirPrefix+suffix)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("%w: environment directory %q already exists", errUnsafeEnvFile, dir)
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", "", err
	}
	cleanupDir = dir
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	activeEnvFiles.Store(dir, &activeEnvFile{
		lock:     lock,
		dirInfo:  dirInfo,
		rootInfo: rootInfo,
		root:     root,
		path:     dir,
		original: dir,
	})
	keep = true
	path = filepath.Join(dir, envFileName)
	return path, dir, nil
}

func writeEnvMarker(path, contents string) (err error) {
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
	if err := writeAll(f, contents); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		f = nil
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

// cleanupEnvFile releases the writer lock and removes the complete private
// directory. It is idempotent. Removal and lock-close failures are returned;
// callers also retain the directory for a deferred retry.
func cleanupEnvFile(dir string) error {
	return cleanupEnvFileContext(context.Background(), dir)
}

func cleanupEnvFileContext(ctx context.Context, dir string) error {
	return cleanupEnvFileAtContext(ctx, "", dir, removeExpectedEnvChildren)
}

func cleanupEnvFileAt(base, dir string, removeAll func(string) error) error {
	return cleanupEnvFileAtContext(context.Background(), base, dir, removeAll)
}

func cleanupEnvFileAtContext(ctx context.Context, base, dir string, removeAll func(string) error) error {
	return cleanupEnvFileAtWithCloseContext(ctx, base, dir, removeAll, closeEnvFileLock)
}

func cleanupEnvFileAtWithClose(base, dir string, removeAll func(string) error, closeLock func(*os.File) error) error {
	return cleanupEnvFileAtWithCloseContext(context.Background(), base, dir, removeAll, closeLock)
}

func cleanupEnvFileAtWithCloseContext(ctx context.Context, base, dir string, removeAll func(string) error, closeLock func(*os.File) error) error {
	if dir == "" {
		return nil
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	if removeAll == nil {
		return errors.New("environment cleanup: nil remove function")
	}
	if closeLock == nil {
		return errors.New("environment cleanup: nil lock close function")
	}

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
	pathMissing := false
	removeStarted := false
	renamed := false
	cleanupErr := withEnvFileRootContext(ctx, base, func(root string) error {
		if err := validateEnvCleanupPath(root, dir); err != nil {
			return err
		}
		ownedState := state.lock != nil || state.pending || state.dirInfo != nil || state.root != ""
		currentRootInfo, err := os.Lstat(root)
		if err != nil {
			if ownedState {
				return fmt.Errorf("%w: environment root %q is unavailable: %v", errUnsafeEnvFile, root, err)
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
		state.rootInfo = currentRootInfo
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			// A pending state whose lock was already released may have
			// completed removal before a late error was reported. An active
			// owned directory, however, cannot disappear and be treated as a
			// successful cleanup: the root or directory was replaced.
			if state.lock != nil || (ownedState && !state.pending) {
				return fmt.Errorf("%w: environment directory %q disappeared while cleanup ownership was active", errUnsafeEnvFile, dir)
			}
			pathMissing = true
			return nil
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
		if state.dirInfo != nil && !os.SameFile(state.dirInfo, info) {
			return fmt.Errorf("%w: environment directory %q was replaced", errUnsafeEnvFile, dir)
		}
		state.root = root
		state.dirInfo = info
		state.path = dir
		if state.original == "" {
			state.original = original
		}

		if state.lock != nil {
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
		} else if state.pending {
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
			}
		} else {
			return fmt.Errorf("environment cleanup: directory %q has no owned lock", dir)
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
				if tombstoneInfo, statErr := os.Lstat(dir); statErr == nil {
					state.dirInfo = tombstoneInfo
				}
			}
			if renameErr != nil {
				return renameErr
			}
		}

		removeStarted = true
		return removeAll(dir)
	})

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
			state.dirInfo = info
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if state != nil {
		state.path = dir
		if cleanupErr == nil && removeStarted && dirExists {
			cleanupErr = fmt.Errorf("environment cleanup: directory %q remains after removal", dir)
		}
		if cleanupErr == nil && !dirExists {
			if stateLocked {
				clearEnvCleanupStateLocked(state)
			} else {
				clearEnvCleanupState(state)
			}
		} else if state.root != "" {
			if removeStarted || renamed {
				state.pending = true
				pendingEnvCleanups.Store(state.original, state)
				if state.path != state.original {
					pendingEnvCleanups.Store(state.path, state)
				}
				activeEnvFiles.Delete(state.original)
				if state.path != state.original {
					activeEnvFiles.Delete(state.path)
				}
			} else if wasPending {
				// A pending retry that still cannot make progress stays
				// pending; it must not be downgraded to a live writer.
				pendingEnvCleanups.Store(state.original, state)
				if state.path != state.original {
					pendingEnvCleanups.Store(state.path, state)
				}
				activeEnvFiles.Delete(state.original)
				if state.path != state.original {
					activeEnvFiles.Delete(state.path)
				}
			} else {
				// Validation failed before the hand-off. Keep the live
				// writer state intact rather than weakening marker checks on
				// a later retry.
				activeEnvFiles.Store(state.original, state)
				pendingEnvCleanups.Delete(state.original)
			}
		}
	}
	return cleanupErr
}

func cleanupEnvFileWithRetry(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), envCleanupDeferredTimeout)
	defer cancel()
	return cleanupEnvFileWithRetryContext(ctx, "", dir)
}

func cleanupEnvFileAfterUseContext(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, envCleanupDeferredTimeout)
	defer cancel()
	return cleanupEnvFileWithRetryContext(cleanupCtx, "", dir)
}

func cleanupEnvFileWithRetryContext(ctx context.Context, base, dir string) error {
	return cleanupEnvFileAtWithRetryContext(ctx, base, dir, removeExpectedEnvChildren)
}

func cleanupEnvFileAtWithRetryContext(ctx context.Context, base, dir string, removeAll func(string) error) error {
	if dir == "" {
		return nil
	}
	// Cleanup is a safety obligation, not ordinary caller work. Keep the
	// caller's cancellation/deadline for the foreground attempt, while the
	// deferred/background retry supplies a detached bounded context.
	retryCtx, cancel := context.WithTimeout(ctx, envCleanupBackgroundTimeout)
	defer cancel()
	var errs []error
	attempts := envCleanupRetryAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := cleanupEnvFileAtContext(retryCtx, base, dir, removeAll); err == nil {
			return nil
		} else {
			errs = append(errs, err)
		}
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

//nolint:unused
func retryEnvFileCleanup(dir *string) {
	_ = retryEnvFileCleanupWithError(dir)
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
	err := cleanupEnvFileAtWithRetryContext(retryCtx, base, value, removeExpectedEnvChildren)
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
			var err error
			if base == "" {
				err = cleanupEnvFileContext(ctx, dir)
			} else {
				err = cleanupEnvFileAtContext(ctx, base, dir, removeExpectedEnvChildren)
			}
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

//nolint:unused
func snapshotPendingEnvCleanups() []pendingEnvCleanup {
	pending, _ := snapshotPendingEnvCleanupsContext(context.Background())
	return pending
}

//nolint:unused
func snapshotPendingEnvCleanupsContext(ctx context.Context) ([]pendingEnvCleanup, error) {
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	if err := lockCleanupEnv(ctx); err != nil {
		return nil, err
	}
	defer cleanupEnvMu.Unlock()
	return snapshotPendingEnvCleanupsLocked(), nil
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
		var err error
		if item.base == "" {
			err = cleanupEnvFileContext(ctx, item.dir)
		} else {
			err = cleanupEnvFileAtContext(ctx, item.base, item.dir, removeExpectedEnvChildren)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &pendingEnvCleanupError{err: errors.Join(errs...)}
}

func joinEnvFileCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, cleanupErr)
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
	if state.original != "" {
		activeEnvFiles.Delete(state.original)
		pendingEnvCleanups.Delete(state.original)
	}
	if state.path != "" {
		activeEnvFiles.Delete(state.path)
		pendingEnvCleanups.Delete(state.path)
	}
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
	scanErr := withEnvFileRootContext(ctx, base, func(root string) error {
		return cleanupEnvDirsContext(ctx, root)
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
	return withEnvFileRootContext(context.Background(), base, fn)
}

func withEnvFileRootWithClose(base string, fn func(string) error, closeRoot func(*os.File) error) (err error) {
	return withEnvFileRootContextWithClose(context.Background(), base, fn, closeRoot)
}

func withEnvFileRootContext(ctx context.Context, base string, fn func(string) error) error {
	return withEnvFileRootContextWithClose(ctx, base, fn, closeEnvFileLock)
}

func withEnvFileRootContextWithClose(ctx context.Context, base string, fn func(string) error, closeRoot func(*os.File) error) (err error) {
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
	if _, err := validateHeldRootMarker(marker, markerPath); err != nil {
		return err
	}
	if err := validatePrivateDirectory(root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(root)
}

func boundedEnvContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, envFileSecurityTimeout)
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

// openOrCreateRootMarker is retained for package-local callers that do not
// need cancellation. New filesystem operations should use the context form.
//
//nolint:unused
func openOrCreateRootMarker(root, markerPath string) (*os.File, os.FileInfo, error) {
	return openOrCreateRootMarkerContext(context.Background(), root, markerPath)
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

// createRootMarker is retained for package-local callers that do not need
// cancellation. New initialization paths use the context form.
//
//nolint:unused
func createRootMarker(path string) (*os.File, os.FileInfo, error) {
	return createRootMarkerContext(context.Background(), path)
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

//nolint:unused
func cleanupEnvDirs(root string) error {
	return cleanupEnvDirsContext(context.Background(), root)
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
	if err := validateExpectedEnvChildren(dir); err != nil {
		return err
	}
	tombstone := dir
	if !strings.HasPrefix(filepath.Base(dir), envFileTombstonePrefix) {
		tombstone, err = renameEnvDirToTombstone(dir, dirInfo)
		if err != nil {
			return err
		}
	}
	return removeExpectedEnvChildren(tombstone)
}

func removeStaleTombstoneEnvDir(dir string, dirInfo os.FileInfo) (err error) {
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	if err := validateExpectedEnvChildren(dir); err != nil {
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
		return removeExpectedEnvChildren(dir)
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
	return removeExpectedEnvChildren(dir)
}

func removeStaleStagingEnvDir(dir string, dirInfo os.FileInfo) error {
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if err := validateExpectedEnvChildren(dir); err != nil {
		return err
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	_, markerErr := os.Lstat(markerPath)
	if errors.Is(markerErr, os.ErrNotExist) {
		if len(entries) != 0 {
			return fmt.Errorf("%w: unmarked staging directory %q contains unexpected children", errUnsafeEnvFile, dir)
		}
		current, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !os.SameFile(dirInfo, current) {
			return fmt.Errorf("%w: staging directory %q was replaced", errUnsafeEnvFile, dir)
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
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
		return removeExpectedEnvChildren(tombstone)
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

func validateExpectedEnvChildren(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectoryInfo(dir, info); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		envFileName:          true,
		envFileLockName:      true,
		envFileDirMarkerName: true,
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("%w: environment directory %q contains unexpected child %q", errUnsafeEnvFile, dir, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: environment child %q is not a regular file", errUnsafeEnvFile, path)
		}
		if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
			return err
		}
	}
	return nil
}

func removeExpectedEnvChildren(dir string) error {
	// Validate the complete allowlist first. Never use RemoveAll here:
	// a foreign child added after validation must make cleanup fail
	// closed rather than be recursively deleted.
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	if err := validateExpectedEnvChildren(dir); err != nil {
		return err
	}
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	current, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !os.SameFile(dirInfo, current) {
		return fmt.Errorf("%w: environment directory %q was replaced during removal", errUnsafeEnvFile, dir)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validatePrivateDirectoryInfo(path, info)
}

func validatePrivateDirectoryInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", errUnsafeEnvFile, path)
	}
	if err := validateEnvOwnership(info, currentEnvFileUID(), envDirMode); err != nil {
		return fmt.Errorf("%w: %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func validatePrivateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validatePrivateRegularInfo(path, info)
}

func validatePrivateRegularInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file", errUnsafeEnvFile, path)
	}
	if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
		return fmt.Errorf("%w: %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func validateEnvOwnership(info os.FileInfo, wantOwner uint32, wantMode os.FileMode) error {
	owner, ok := envFileUID(info)
	if !ok || owner != wantOwner {
		return fmt.Errorf("owner does not match the current user")
	}
	if info.Mode().Perm() != wantMode.Perm() {
		return fmt.Errorf("mode is %04o, want %04o", info.Mode().Perm(), wantMode.Perm())
	}
	return nil
}

func validateEnvMarkerIfPresent(dir string) error {
	marker, _, err := openValidatedEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return marker.Close()
}

func repairEnvMarker(path, contents string) (err error) {
	f, err := openEnvFileNoFollow(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: environment marker %q is unsafe: %v", errUnsafeEnvFile, path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		return fmt.Errorf("%w: environment marker %q is unsafe: %v", errUnsafeEnvFile, path, err)
	}
	if info.Size() > int64(len(contents)) {
		return fmt.Errorf("%w: environment marker %q is too long", errUnsafeEnvFile, path)
	}
	data := make([]byte, info.Size())
	if len(data) > 0 {
		n, readErr := f.ReadAt(data, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != len(data) || !strings.HasPrefix(contents, string(data)) {
			return fmt.Errorf("%w: environment marker %q has unsafe contents", errUnsafeEnvFile, path)
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return fmt.Errorf("%w: environment marker %q was replaced", errUnsafeEnvFile, path)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return writeAll(f, contents)
}

func openValidatedEnvMarker(path, contents string, flag int) (*os.File, os.FileInfo, error) {
	f, err := openEnvFileNoFollow(path, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if info.Size() != int64(len(contents)) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q has invalid length", errUnsafeEnvFile, path)
	}
	buf := make([]byte, len(contents))
	n, readErr := f.ReadAt(buf, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		_ = f.Close()
		return nil, nil, readErr
	}
	if n != len(contents) || string(buf) != contents {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q has invalid contents", errUnsafeEnvFile, path)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q was replaced", errUnsafeEnvFile, path)
	}
	return f, info, nil
}

func openValidatedPrivateFile(path string) (*os.File, os.FileInfo, error) {
	f, err := openEnvFileNoFollow(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: file %q was replaced", errUnsafeEnvFile, path)
	}
	return f, info, nil
}
