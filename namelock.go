//go:build !windows

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	nameLockDirPerm  = 0o700
	nameLockFilePerm = 0o600
	nameLockPoll     = 10 * time.Millisecond
)

// nameLockPath returns the durable, account-scoped path for name. The
// digest keeps arbitrary container names out of the filesystem path and
// avoids both path traversal and filename-length surprises.
func nameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	dir, err := nameLockDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, nameLockFileName(name)), nil
}

func nameLockFileName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:]) + ".lock"
}

// nameLockDir returns a private directory below the account's native
// durable state location. It deliberately ignores HOME, XDG_CACHE_HOME,
// and XDG_STATE_HOME: cooperating processes may launch with different
// values but must select the same host/user lock namespace.
func nameLockDir() (string, error) {
	stateDir, err := accountStateDir()
	if err != nil {
		return "", err
	}
	appDir := filepath.Join(stateDir, "container-go")
	lockDir := filepath.Join(appDir, "locks")
	for _, dir := range []string{appDir, lockDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return "", err
		}
	}
	return lockDir, nil
}

func accountStateDir() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("look up current user: %w", err)
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", fmt.Errorf("current user home %q is not absolute", current.HomeDir)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(current.HomeDir, "Library", "Application Support"), nil
	}
	return filepath.Join(current.HomeDir, ".local", "state"), nil
}

// transitionalNameLockPath is the environment-selected UserCacheDir
// namespace introduced before the durable account-state namespace. It is
// retained as a compatibility barrier, not as the canonical lock path.
func transitionalNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find transitional user cache directory: %w", err)
	}
	appDir := filepath.Join(cacheDir, "container-go")
	lockDir := filepath.Join(appDir, "locks")
	for _, dir := range []string{appDir, lockDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return "", err
		}
	}
	return filepath.Join(lockDir, nameLockFileName(name)), nil
}

// legacyNameLockPath is the original TMPDIR namespace. Keeping this as
// the first barrier lets a new process coordinate with binaries from
// before the UserCacheDir migration.
func legacyNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	tempDir := os.TempDir()
	if !filepath.IsAbs(tempDir) {
		return "", fmt.Errorf("temporary directory %q is not absolute", tempDir)
	}
	return filepath.Join(tempDir, "containergo-"+name+".lock"), nil
}

// ensurePrivateDir creates dir if needed and refuses to use a path that
// could have been substituted by another user. The directory is kept
// persistent; removing it would let a new inode bypass an existing flock.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, nameLockDirPerm); err != nil {
		return fmt.Errorf("create lock directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat lock directory %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock directory %s is not a directory", dir)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		return fmt.Errorf("lock directory %s has permissions %04o, want %04o", dir, info.Mode().Perm(), nameLockDirPerm)
	}
	return checkLockOwner(info, "lock directory")
}

func checkLockOwner(info os.FileInfo, kind string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s ownership is unavailable", kind)
	}
	if uid := uint32(os.Geteuid()); stat.Uid != uid {
		return fmt.Errorf("%s is owned by uid %d, want uid %d", kind, stat.Uid, uid)
	}
	return nil
}

func checkLockFile(info os.FileInfo, path string) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock file %s is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lock file %s is not a regular file", path)
	}
	if info.Mode().Perm() != nameLockFilePerm {
		return fmt.Errorf("lock file %s has permissions %04o, want %04o", path, info.Mode().Perm(), nameLockFilePerm)
	}
	return checkLockOwner(info, "lock file")
}

// openNameLockPath opens the stable lock inode after checking that it is a
// private, regular file. Keeping this preparation separate lets the reaper
// validate and create the same file that lockName will later use.
func openNameLockPath(path string) (*os.File, error) {
	// Check an existing path before opening it. O_NOFOLLOW below closes
	// the race with a symlink being installed between this check and open;
	// the post-open fstat also protects against replacement by a regular
	// file owned by another user.
	if info, statErr := os.Lstat(path); statErr == nil {
		if err := checkLockFile(info, path); err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat lock file %s: %w", path, statErr)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, nameLockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := checkOpenedNameLockFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkOpenedNameLockFile(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat open lock file %s: %w", path, err)
	}
	if err := checkLockFile(opened, path); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck lock file %s: %w", path, err)
	}
	if err := checkLockFile(current, path); err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("lock file %s was replaced while opening", path)
	}
	return nil
}

type resolvedNameLocks struct {
	legacy       string
	transitional string
	state        string
}

func (l resolvedNameLocks) ordered() []string {
	return []string{l.legacy, l.transitional, l.state}
}

func resolveNameLocks(name string) (resolvedNameLocks, error) {
	legacy, err := legacyNameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve legacy lock: %w", err)
	}
	transitional, err := transitionalNameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve transitional lock: %w", err)
	}
	state, err := nameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve durable lock: %w", err)
	}
	return resolvedNameLocks{legacy: legacy, transitional: transitional, state: state}, nil
}

// reaperNameLockPaths prepares every lock inode before the shell reaper
// receives it. The reaper protocol is fail-closed and acquires the paths
// in the same legacy-to-durable order as lockName.
func reaperNameLockPaths(name string) ([]string, error) {
	resolved, err := resolveNameLocks(name)
	if err != nil {
		return nil, err
	}
	paths := resolved.ordered()
	for _, path := range paths {
		f, err := openNameLockPath(path)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

type nameLockHooks struct {
	afterAcquire func(string)
}

func acquireNameLockFile(ctx context.Context, path string, hooks *nameLockHooks) (func(), error) {
	f, err := openNameLockPath(path)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(nameLockPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := tryLockNameFile(f)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	if hooks != nil && hooks.afterAcquire != nil {
		hooks.afterAcquire(path)
	}
	// Recheck after flock acquisition as well as after open. A path
	// replacement while the caller owns the file must never let it enter
	// a critical section through an inode that no longer has the
	// requested name.
	if err := checkOpenedNameLockFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockNameFile(f)
			_ = f.Close()
		})
	}, nil
}

type processNameLock struct {
	token      chan struct{}
	references int
}

var processNameLocks = struct {
	sync.Mutex
	byName map[string]*processNameLock
}{
	byName: make(map[string]*processNameLock),
}

// acquireProcessNameLock serializes goroutines before they contend for the
// file lock. Darwin flock ownership is process-associated, so separate
// goroutines must not rely on the kernel to exclude one another.
func acquireProcessNameLock(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	processNameLocks.Lock()
	lock := processNameLocks.byName[name]
	if lock == nil {
		lock = &processNameLock{token: make(chan struct{}, 1)}
		processNameLocks.byName[name] = lock
	}
	lock.references++
	processNameLocks.Unlock()

	releaseReference := func() {
		processNameLocks.Lock()
		lock.references--
		if lock.references == 0 && processNameLocks.byName[name] == lock {
			delete(processNameLocks.byName, name)
		}
		processNameLocks.Unlock()
	}
	select {
	case lock.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock.token
			releaseReference()
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				<-lock.token
				releaseReference()
			})
		}, nil
	case <-ctx.Done():
		releaseReference()
		return nil, ctx.Err()
	}
}

// lockName serializes generation-checked, name-addressed creates and
// deletes of one container name across goroutines and processes on this
// host. Apple Container has no immutable container ID, so an
// inspect-then-delete by name is only safe if no other cooperating actor
// can mutate the name in between; the guarded create and delete paths in
// this library take this lock first. That covers cooperating processes
// using this guarded protocol only. A direct `container` CLI invocation,
// an unguarded library operation, or another external actor does not take
// the lock and can change the state, labels, generation, or name target
// after inspect and before delete. Closing that would need an immutable ID
// or an atomic conditional delete from the backend, which Apple Container
// does not offer.
//
// Lock files are intentionally persistent. flock locks are released by
// the kernel when a process exits, so a leftover file is stale but
// reusable rather than a reason to unlink it; unlinking would permit a
// second process to lock a different inode while the first still holds
// the old one.
func lockName(ctx context.Context, name string) (func(), error) {
	return lockNameWithHooks(ctx, name, nil)
}

func lockNameWithHooks(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, queryTimeout)
		defer cancel()
	}
	unlockProcess, err := acquireProcessNameLock(ctx, name)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveNameLocks(name)
	if err != nil {
		unlockProcess()
		return nil, err
	}

	paths := resolved.ordered()
	stages := []string{"legacy", "transitional", "durable"}
	acquired := make([]func(), 0, len(paths))
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	for i, path := range paths {
		unlock, err := acquireNameLockFile(ctx, path, hooks)
		if err != nil {
			release()
			unlockProcess()
			return nil, fmt.Errorf("acquire %s name lock: %w", stages[i], err)
		}
		acquired = append(acquired, unlock)
	}
	if err := ctx.Err(); err != nil {
		release()
		unlockProcess()
		return nil, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			unlockProcess()
		})
	}, nil
}

// nameLockProtocolPath is kept small and explicit because the shell reaper
// receives the path as data. Rejecting control characters prevents a path
// from changing the tab/newline-delimited reaper protocol.
func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
