//go:build windows

package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Windows environment files live under
//
//	%LocalAppData%\containergo-env-v1\containergo-env-<32 hex>\
//	    .container-go-env   marker
//	    .lock               writer lock held for the backend call
//	    env                 the secret file passed to --env-file
//
// Every directory and file is created with a protected DACL that grants only
// the current logon (plus Administrators and LOCAL SYSTEM, as the staging
// implementation does) and is revalidated before use and before removal. The
// root `.lock` serializes creation and stale scans; the per-directory `.lock`
// is how a stale scan distinguishes a live writer from a crashed process.

type windowsEnvFileState struct {
	lock *os.File
}

// setLock and takeLock keep the lock descriptor access synchronized with the
// registry, which a later stale scan or deferred cleanup may read from another
// goroutine.
func (s *windowsEnvFileState) setLock(f *os.File) {
	windowsEnvMu.Lock()
	s.lock = f
	windowsEnvMu.Unlock()
}

func (s *windowsEnvFileState) getLock() *os.File {
	windowsEnvMu.Lock()
	defer windowsEnvMu.Unlock()
	return s.lock
}

func (s *windowsEnvFileState) takeLock() *os.File {
	windowsEnvMu.Lock()
	defer windowsEnvMu.Unlock()
	f := s.lock
	s.lock = nil
	return f
}

var (
	windowsEnvMu    sync.Mutex
	windowsEnvFiles = make(map[string]*windowsEnvFileState)
)

func storeWindowsEnvState(dir string, state *windowsEnvFileState) {
	windowsEnvMu.Lock()
	windowsEnvFiles[dir] = state
	windowsEnvMu.Unlock()
}

func loadWindowsEnvState(dir string) *windowsEnvFileState {
	windowsEnvMu.Lock()
	defer windowsEnvMu.Unlock()
	return windowsEnvFiles[dir]
}

func takeWindowsEnvState(dir string) *windowsEnvFileState {
	windowsEnvMu.Lock()
	defer windowsEnvMu.Unlock()
	state := windowsEnvFiles[dir]
	delete(windowsEnvFiles, dir)
	return state
}

const (
	windowsEnvCleanupInitialBackoff = 5 * time.Millisecond
	windowsEnvCleanupMaxBackoff     = 250 * time.Millisecond
)

var (
	// Deletion can transiently fail while antivirus or the search indexer
	// holds a child open. The bounded backoff keeps a failed cleanup from
	// failing a successful backend call immediately, without hiding a real
	// permission problem for long. A variable so tests can shorten it.
	windowsEnvCleanupTimeout = 2 * time.Second
)

// errWindowsEnvCleanupBusy reports that a stale scan holds the writer lock
// and is reclaiming the directory. The caller retries briefly and only
// reports success once the directory is actually gone.
var errWindowsEnvCleanupBusy = errors.New("environment directory is being reclaimed by a stale scan")

// Test hook: runs while the owner's writer lock is still held and the env
// file and marker are gone, before the lock file is removed. Tests that set
// it must not use t.Parallel.
var windowsEnvCleanupBeforeLockCloseHook func(string)

// WithEnv on Windows: the secret file is created with a DACL that is limited
// to this logon, under a root that is revalidated on every use.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	return writeEnvFileContext(context.Background(), env)
}

func writeEnvFileContext(ctx context.Context, env map[string]string) (path, dir string, err error) {
	return writeEnvFileAtContext(ctx, "", env)
}

func writeEnvFileAtContext(ctx context.Context, base string, env map[string]string) (path, dir string, err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	err = withEnvFileRootContext(ctx, base, func(rootCtx context.Context, root string) error {
		if err := cleanupWindowsEnvDirsIfDueContext(rootCtx, root); err != nil {
			return err
		}
		path, dir, err = createWindowsEnvFileContext(rootCtx, root, env)
		return err
	})
	if err == nil {
		return path, dir, nil
	}
	if dir != "" {
		// The create path published an owned directory that could not be
		// removed. Retry cleanup here; retain the path only when the caller
		// must perform a later retry.
		retryCtx, retryCancel := context.WithTimeout(context.WithoutCancel(ctx), windowsEnvCleanupTimeout)
		cleanupErr := retryWindowsEnvCleanup(retryCtx, dir)
		retryCancel()
		if cleanupErr != nil {
			return path, dir, errors.Join(err, cleanupErr)
		}
	}
	return "", "", err
}

// withEnvFileRoot runs fn while holding the root lock, so creation and stale
// scans never observe each other's partially created entries.
func withEnvFileRoot(base string, fn func(string) error) error {
	if fn == nil {
		return errors.New("environment root: nil callback")
	}
	return withEnvFileRootContext(context.Background(), base, func(_ context.Context, root string) error {
		return fn(root)
	})
}

func withEnvFileRootContext(ctx context.Context, base string, fn func(context.Context, string) error) (err error) {
	if fn == nil {
		return errors.New("environment root: nil callback")
	}
	root, err := ensureWindowsEnvRoot(base)
	if err != nil {
		return err
	}
	lock, err := openWindowsEnvRootLock(root)
	if err != nil {
		return err
	}
	if err := acquireWindowsRootLockContext(ctx, lock); err != nil {
		return errors.Join(err, lock.Close())
	}
	callbackErr := fn(ctx, root)
	releaseErr := releaseWindowsFileLock(lock)
	closeErr := lock.Close()
	if callbackErr != nil {
		return errors.Join(callbackErr, releaseErr, closeErr)
	}
	if releaseErr != nil || closeErr != nil {
		return errors.Join(releaseErr, closeErr)
	}
	return ctx.Err()
}

func createWindowsEnvFileContext(ctx context.Context, root string, env map[string]string) (path, dir string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	dir, err = createWindowsEnvDir(root)
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, envFileName)
	state := &windowsEnvFileState{}
	storeWindowsEnvState(dir, state)
	// abort removes whatever was published. It returns the directory only
	// when that cleanup also failed, which transfers retry ownership to the
	// caller.
	abort := func(cause error) (string, string, error) {
		cleanupErr := cleanupWindowsEnvFile(dir)
		if cleanupErr != nil {
			return path, dir, errors.Join(cause, cleanupErr)
		}
		return "", "", cause
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	if err := createWindowsPrivateFile(markerPath, []byte(envFileDirMarker)); err != nil {
		return abort(err)
	}
	lock, err := createWindowsEnvLock(dir)
	if err != nil {
		return abort(err)
	}
	state.setLock(lock)
	locked, err := tryAcquireWindowsFileLock(lock)
	if err != nil {
		return abort(err)
	}
	if !locked {
		return abort(errors.New("environment file lock is already held"))
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}
	contents := windowsEnvFileContents(env)
	defer clear(contents)
	if err := createWindowsPrivateFile(path, contents); err != nil {
		return abort(err)
	}
	if err := ctx.Err(); err != nil {
		return abort(err)
	}
	if err := verifyWindowsEnvDirContents(dir, true); err != nil {
		return abort(err)
	}
	return path, dir, nil
}

func windowsEnvFileContents(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, env[k]...)
		b = append(b, '\n')
	}
	return b
}

func createWindowsEnvDir(root string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(root, envFileDirPrefix+hex.EncodeToString(random[:]))
		attributes, err := currentUserSecurityAttributes()
		if err != nil {
			return "", err
		}
		pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(dir))
		if err != nil {
			return "", err
		}
		err = windows.CreateDirectory(pathPtr, attributes)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return "", &os.PathError{Op: "mkdir", Path: dir, Err: err}
		}
		if err := configureWindowsPrivateDir(dir, false, true); err != nil {
			_ = os.Remove(dir)
			return "", err
		}
		return dir, nil
	}
	return "", fmt.Errorf("%w: could not allocate a private environment directory", errUnsafeEnvFile)
}

func createWindowsEnvLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, envFileLockName)
	f, err := createWindowsPrivateFileHandle(path, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		return nil, err
	}
	if err := hardenWindowsPrivateFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// cleanupWindowsEnvFile removes a directory this process created. The writer
// lock is held while the children are removed, so a concurrent stale scan
// cannot observe a half-removed directory: it fails to take the lock and
// skips the entry. Removal is sequential (env, marker, then the lock file)
// and stops at the first failure, so a transient sharing violation never
// leaves a directory without a marker. On failure the registry entry and its
// writer lock are retained for the retry, keeping the invariant visible to
// scan: an env file always has its marker, and the marker is gone only after
// the env file. If cleanup gives up, the writer lock is released so the next
// stale scan can reclaim the directory (or report it fail-closed). The lock
// file is deleted while its handle is still open (the handle shares delete),
// then the handle is closed and the now-empty directory is removed.
func cleanupWindowsEnvFile(dir string) error {
	state, _, err := acquireWindowsEnvCleanupLock(dir)
	if errors.Is(err, errWindowsEnvCleanupBusy) {
		// Let the scan finish and retry shortly; cleanup does not report
		// success until the directory is gone.
		return err
	}
	if err != nil {
		return err
	}

	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return releaseWindowsEnvCleanupState(dir, state)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: environment directory %q is not a real directory", errUnsafeEnvFile, dir)
	}
	if err := verifyWindowsEnvDirContents(dir, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return releaseWindowsEnvCleanupState(dir, state)
		}
		return err
	}
	if err := removeWindowsEnvChild(dir, envFileName); err != nil {
		return err
	}
	if err := removeWindowsEnvChild(dir, envFileDirMarkerName); err != nil {
		return err
	}
	if hook := windowsEnvCleanupBeforeLockCloseHook; hook != nil {
		hook(dir)
	}
	// The lock file is removed while its handle is open, so the writer lock
	// stays held through the last child deletion.
	if err := removeWindowsEnvChild(dir, envFileLockName); err != nil {
		return err
	}
	if err := releaseWindowsEnvCleanupState(dir, state); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// acquireWindowsEnvCleanupLock returns the writer lock for dir. A retry reuses
// the registry entry so the same lock stays held between attempts; a missing
// lock file is a partially created directory that is removed without one.
func acquireWindowsEnvCleanupLock(dir string) (*windowsEnvFileState, *os.File, error) {
	if state := loadWindowsEnvState(dir); state != nil {
		if lock := state.getLock(); lock != nil {
			return state, lock, nil
		}
	}
	lockPath := filepath.Join(dir, envFileLockName)
	lock, err := openWindowsPrivateFileHandle(lockPath, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := verifyWindowsPrivateFileHandle(lock, lockPath); err != nil {
		_ = lock.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	locked, err := tryAcquireWindowsFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	if !locked {
		// A stale scan owns the removal. Closing here is safe: the scan holds
		// the lock, so the retry observes either the busy lock or the
		// finished directory.
		return nil, nil, errors.Join(errWindowsEnvCleanupBusy, lock.Close())
	}
	state := &windowsEnvFileState{}
	state.setLock(lock)
	storeWindowsEnvState(dir, state)
	return state, lock, nil
}

// releaseWindowsEnvCleanupState unregisters the directory and closes the
// writer lock. It is called only once the directory's children are gone.
func releaseWindowsEnvCleanupState(dir string, state *windowsEnvFileState) error {
	if state == nil {
		return nil
	}
	takeWindowsEnvState(dir)
	if lock := state.takeLock(); lock != nil {
		return lock.Close()
	}
	return nil
}

// removeWindowsEnvChild deletes one expected child. A concurrent removal by a
// stale scan is not an error: the directory is on its way out either way.
func removeWindowsEnvChild(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// verifyWindowsEnvDirContents rejects unexpected children and validates the
// DACL of every entry. requireMarker is false only for cleanup of a directory
// this process created, which may have failed before the marker was written.
func verifyWindowsEnvDirContents(dir string, requireMarker bool) error {
	if err := verifyWindowsPrivateDir(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		switch entry.Name() {
		case envFileName, envFileLockName, envFileDirMarkerName:
			seen[entry.Name()] = true
		default:
			return fmt.Errorf("%w: environment directory %q contains unexpected entry %q", errUnsafeEnvFile, dir, entry.Name())
		}
	}
	markerValid := false
	if seen[envFileDirMarkerName] {
		data, err := readWindowsPrivateFile(filepath.Join(dir, envFileDirMarkerName))
		if err != nil {
			return err
		}
		markerValid = string(data) == envFileDirMarker
		// requireMarker is true only for the post-creation check. Removal
		// (owner cleanup and stale reclaim alike) tolerates a missing or
		// partial marker, because a directory that only retains the lock
		// file or a torn marker holds no env file. The env-to-marker rule
		// below is what keeps a secret-bearing directory from being deleted
		// in a torn state.
		if requireMarker && !markerValid {
			return fmt.Errorf("%w: environment directory %q has an invalid marker", errUnsafeEnvFile, dir)
		}
	} else if requireMarker {
		return fmt.Errorf("%w: environment directory %q has no marker", errUnsafeEnvFile, dir)
	}
	if seen[envFileName] {
		// The env file is written after the marker and the marker is removed
		// after the env file, so an env file without its complete marker is a
		// torn state that must fail closed rather than be deleted.
		if !markerValid {
			return fmt.Errorf("%w: environment directory %q has an env file without a valid marker", errUnsafeEnvFile, dir)
		}
		if err := verifyWindowsPrivateFile(filepath.Join(dir, envFileName)); err != nil {
			return err
		}
	}
	if seen[envFileLockName] {
		if err := verifyWindowsPrivateFile(filepath.Join(dir, envFileLockName)); err != nil {
			return err
		}
	}
	return nil
}

func removeWindowsEnvDirectory(dir string) error {
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		if err := removeWindowsEnvChild(dir, name); err != nil {
			return err
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func cleanupStaleEnvFilesContext(ctx context.Context) error {
	return cleanupStaleEnvFilesAtContext(ctx, "")
}

func cleanupStaleEnvFilesAtContext(ctx context.Context, base string) error {
	if err := ensureEnvFileSecurity(); err != nil {
		return err
	}
	ctx, cancel := boundedEnvContext(ctx)
	defer cancel()
	return withEnvFileRootContext(ctx, base, func(rootCtx context.Context, root string) error {
		if err := cleanupWindowsEnvDirsContext(rootCtx, root); err != nil {
			return err
		}
		markEnvDirGC(root, time.Now())
		return nil
	})
}

// cleanupWindowsEnvDirsIfDueContext reclaims abandoned directories at most
// once per envDirGCInterval for root, matching the Unix write path. Run's
// explicit security preflight calls cleanupWindowsEnvDirsContext directly and
// stays unconditional. A failed scan is not recorded, so the entry that made
// it fail is retried by the next write.
func cleanupWindowsEnvDirsIfDueContext(ctx context.Context, root string) error {
	now := time.Now()
	if !envDirGCDue(root, now) {
		return nil
	}
	if err := cleanupWindowsEnvDirsContext(ctx, root); err != nil {
		return err
	}
	markEnvDirGC(root, now)
	return nil
}

func cleanupWindowsEnvDirsContext(ctx context.Context, root string) error {
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
		if !strings.HasPrefix(name, envFileDirPrefix) {
			continue
		}
		dir := filepath.Join(root, name)
		if loadWindowsEnvState(dir) != nil {
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
		if err := reclaimWindowsEnvDir(dir, now.Sub(info.ModTime())); err != nil {
			return err
		}
	}
	return nil
}

// reclaimWindowsEnvDir removes a directory left behind by a crashed process.
// The writer lock decides liveness: if another process still holds it, the
// directory belongs to a live backend call and is skipped. The lock is held
// while the children are removed so a concurrent owner cleanup observes a
// busy lock instead of a half-removed directory. Removal is sequential and
// stops at the first failure; a transient sharing/access failure is treated
// as a skip for this scan so another process's leftover cannot fail an
// unrelated Run. The remaining marker and writer lock keep the directory in
// the normal shape for the next scan.
func reclaimWindowsEnvDir(dir string, age time.Duration) error {
	lockPath := filepath.Join(dir, envFileLockName)
	lock, err := openWindowsPrivateFileHandle(lockPath, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if errors.Is(err, os.ErrNotExist) {
		return reclaimUnlockedWindowsEnvDir(dir, age)
	}
	if err != nil {
		if isWindowsEnvTransientCleanupError(err) {
			return nil
		}
		return err
	}
	if err := verifyWindowsPrivateFileHandle(lock, lockPath); err != nil {
		_ = lock.Close()
		if errors.Is(err, os.ErrNotExist) || isWindowsEnvTransientCleanupError(err) {
			return nil
		}
		return err
	}
	locked, err := tryAcquireWindowsFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !locked {
		// A live writer holds the lock; leave the directory alone.
		return lock.Close()
	}
	if err := verifyWindowsEnvDirContents(dir, false); err != nil {
		_ = lock.Close()
		if errors.Is(err, os.ErrNotExist) || isWindowsEnvTransientCleanupError(err) {
			return nil
		}
		return err
	}
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		if err := removeWindowsEnvChild(dir, name); err != nil {
			_ = lock.Close()
			if isWindowsEnvTransientCleanupError(err) {
				return nil
			}
			return err
		}
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if isWindowsEnvTransientCleanupError(err) {
			return nil
		}
		return err
	}
	return nil
}

func reclaimUnlockedWindowsEnvDir(dir string, age time.Duration) error {
	// A directory without a lock is a crash between CreateDirectory and lock
	// creation. It cannot contain the env file (that is written last), so an
	// age gate keeps a creator that somehow outlives the root lock safe.
	if age < envFileStagingStaleAfter {
		return nil
	}
	if err := verifyWindowsPrivateDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(entries) > 1 {
		return fmt.Errorf("%w: unmarked environment directory %q contains unexpected children", errUnsafeEnvFile, dir)
	}
	if len(entries) == 1 && entries[0].Name() != envFileDirMarkerName {
		return fmt.Errorf("%w: unmarked environment directory %q contains unexpected state", errUnsafeEnvFile, dir)
	}
	if len(entries) == 1 {
		data, err := readWindowsPrivateFile(filepath.Join(dir, envFileDirMarkerName))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		// A crash during the marker write leaves a prefix of the marker. It
		// holds no secret and must not make every later Run fail closed.
		if !strings.HasPrefix(envFileDirMarker, string(data)) {
			return fmt.Errorf("%w: environment directory %q has an invalid marker", errUnsafeEnvFile, dir)
		}
	}
	if err := removeWindowsEnvDirectory(dir); err != nil {
		if isWindowsEnvTransientCleanupError(err) {
			return nil
		}
		return err
	}
	return nil
}

func cleanupEnvFile(dir string) error {
	return cleanupEnvFileWithRetry(dir)
}

func cleanupEnvFileWithRetry(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), windowsEnvCleanupTimeout)
	defer cancel()
	return retryWindowsEnvCleanup(ctx, dir)
}

func cleanupEnvFileAfterUseContext(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowsEnvCleanupTimeout)
	defer cancel()
	return retryWindowsEnvCleanup(cleanupCtx, dir)
}

func retryEnvFileCleanupWithError(dir *string) error {
	if dir == nil || *dir == "" {
		return nil
	}
	value := *dir
	retryCtx, cancel := context.WithTimeout(context.Background(), windowsEnvCleanupTimeout)
	err := retryWindowsEnvCleanup(retryCtx, value)
	cancel()
	if err == nil {
		*dir = ""
		return nil
	}
	log.Printf("container-go: retry environment file cleanup: %v", err)
	return err
}

func retryWindowsEnvCleanup(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	retryCtx, cancel := context.WithTimeout(ctx, envCleanupBackgroundTimeout)
	defer cancel()
	backoff := windowsEnvCleanupInitialBackoff
	for {
		err := cleanupWindowsEnvFile(dir)
		if err == nil {
			return nil
		}
		if errors.Is(err, os.ErrNotExist) {
			// The directory disappeared (for example a stale scan won the
			// race); removal is complete.
			return nil
		}
		if !isWindowsEnvTransientCleanupError(err) {
			releaseWindowsEnvCleanupAfterFailure(dir)
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-retryCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			releaseWindowsEnvCleanupAfterFailure(dir)
			return errors.Join(err, retryCtx.Err())
		case <-timer.C:
		}
		if backoff < windowsEnvCleanupMaxBackoff {
			backoff *= 2
			if backoff > windowsEnvCleanupMaxBackoff {
				backoff = windowsEnvCleanupMaxBackoff
			}
		}
	}
}

// isWindowsEnvTransientCleanupError reports failures that antivirus, the
// search indexer, or another reader can cause for a moment. Validation
// failures are deterministic and are not retried.
func isWindowsEnvTransientCleanupError(err error) bool {
	if errors.Is(err, errUnsafeEnvFile) {
		return false
	}
	return errors.Is(err, errWindowsEnvCleanupBusy) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}

// releaseWindowsEnvCleanupAfterFailure drops the in-process writer lock when
// cleanup gives up: a non-retryable validation failure or an exhausted retry
// budget. Removal is sequential, so the directory still has its valid marker
// whenever the env file remains; that normal shape lets a later stale scan in
// this or another process reclaim it instead of skipping a lock nobody will
// release. A directory that is unsafe to remove is still reported fail-closed
// by that scan.
func releaseWindowsEnvCleanupAfterFailure(dir string) {
	if state := loadWindowsEnvState(dir); state != nil {
		_ = releaseWindowsEnvCleanupState(dir, state)
	}
}

// A Windows cleanup failure is retried with a bounded backoff; on a terminal
// failure the writer lock is released so the next stale scan can reclaim the
// directory. It is never reported as the retryable pending state used by the
// reuse attach path: a failure to remove a secret-bearing directory must fail
// closed.
func isRetryablePendingEnvCleanupError(error) bool {
	return false
}
