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

// ErrNameLockCompatibility identifies a lock namespace that cannot be
// established safely. Callers must fail closed rather than falling back to
// an unlocked namespace.
var ErrNameLockCompatibility = errors.New("container name-lock namespace is incompatible")

// nameLockStateRootOverride is a package test seam. Production leaves it
// empty, so the account database (rather than a mutable environment)
// selects the durable namespace.
var nameLockStateRootOverride string

type nameLockStage string

const (
	legacyNameLockStage       nameLockStage = "legacy"
	transitionalNameLockStage nameLockStage = "transitional"
	stateNameLockStage        nameLockStage = "state"
)

// nameLockHooks makes replacement and acquisition races deterministic in
// package tests without weakening the production path.
type nameLockHooks struct {
	beforeOpen    func(nameLockStage, string)
	afterOpen     func(nameLockStage, string)
	afterAcquire  func(string)
	beforeCleanup func()
}

// nameLockPath returns the durable account-scoped path for name. A digest
// keeps arbitrary names out of the filesystem namespace.
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

// nameLockDir returns a private durable directory below the account's
// native state location. HOME, XDG_STATE_HOME, and XDG_CACHE_HOME do not
// select this namespace: cooperating processes may have different values.
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
	if nameLockStateRootOverride != "" {
		if !filepath.IsAbs(nameLockStateRootOverride) {
			return "", fmt.Errorf("state root %q is not absolute", nameLockStateRootOverride)
		}
		if info, err := os.Lstat(nameLockStateRootOverride); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("state root %s is a symbolic link", nameLockStateRootOverride)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		resolved, err := resolveDirectoryPath(nameLockStateRootOverride)
		if err != nil {
			return "", fmt.Errorf("resolve state root %s: %w", nameLockStateRootOverride, err)
		}
		return resolved, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("look up current user: %w", err)
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", fmt.Errorf("current user home %q is not absolute", current.HomeDir)
	}
	home, err := filepath.EvalSymlinks(current.HomeDir)
	if err != nil {
		return "", fmt.Errorf("resolve current user home: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support"), nil
	}
	return filepath.Join(home, ".local", "state"), nil
}

// transitionalNameLockPath is the UserCacheDir namespace used by an
// intermediate revision. It remains a mandatory compatibility barrier.
func transitionalNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find transitional user cache directory: %w", err)
	}
	if !filepath.IsAbs(cacheDir) {
		return "", fmt.Errorf("transitional cache directory %q is not absolute", cacheDir)
	}
	// UserCacheDir can expose a platform alias such as /var -> /private/var
	// on macOS. Resolve existing aliases before the private namespace walk so
	// the compatibility path still names the same directory inode.
	if resolved, resolveErr := resolveDirectoryPath(cacheDir); resolveErr == nil {
		cacheDir = resolved
	} else {
		return "", fmt.Errorf("resolve transitional cache directory %s: %w", cacheDir, resolveErr)
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

// legacyNameLockPath is the original TMPDIR namespace. It is retained so
// a current process coordinates with callers from before the migration.
func legacyNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	tempDir := os.TempDir()
	if !filepath.IsAbs(tempDir) {
		return "", fmt.Errorf("legacy temporary directory %q is not absolute", tempDir)
	}
	// Resolve aliases such as /var -> /private/var without changing the
	// filename that old callers use. An unresolvable legacy directory is
	// not safe to use as a compatibility barrier.
	resolved, resolveErr := filepath.EvalSymlinks(tempDir)
	if resolveErr != nil {
		return "", fmt.Errorf("resolve legacy temporary directory %s: %w", tempDir, resolveErr)
	}
	tempDir = resolved
	return filepath.Join(tempDir, "containergo-"+name+".lock"), nil
}

// resolveDirectoryPath resolves existing aliases while retaining missing
// trailing components. This permits a first-use cache/state directory to be
// created below a platform alias such as /var without ever following a
// symlink in the private directory itself.
func resolveDirectoryPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func rejectSymlinkComponents(path string) error {
	path = filepath.Clean(path)
	for {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			// A missing trailing component does not make the existing
			// parents safe to inspect; continue walking so a symlink in
			// an already-existing parent is rejected before MkdirAll can
			// follow it.
			parent := filepath.Dir(path)
			if parent == path {
				return nil
			}
			path = parent
			continue
		}
		if err != nil {
			return fmt.Errorf("stat lock path %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("lock path %s is a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("lock path %s is not a directory", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func ensurePrivateDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("lock directory %s is not absolute", dir)
	}
	if err := rejectSymlinkComponents(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, nameLockDirPerm); err != nil {
		return fmt.Errorf("create lock directory %s: %w", dir, err)
	}
	// MkdirAll follows symlinks in existing parents. Walk the completed
	// chain afterwards so a substituted namespace component fails closed.
	current := filepath.Clean(dir)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("stat lock directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("lock directory %s contains symbolic-link component %s", dir, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("lock directory %s contains non-directory component %s", dir, current)
		}
		if current != dir && info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("lock directory %s has group/other-writable component %s", dir, current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat lock directory %s: %w", dir, err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("lock directory %s has special mode bits", dir)
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
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("lock file %s has special mode bits", path)
	}
	if info.Mode().Perm() != nameLockFilePerm {
		return fmt.Errorf("lock file %s has permissions %04o, want %04o", path, info.Mode().Perm(), nameLockFilePerm)
	}
	return checkLockOwner(info, "lock file")
}

// openNameLockPath opens the stable inode with O_NOFOLLOW and verifies the
// opened object and the directory entry are the same owned private file.
func openNameLockPath(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if err := checkLockFile(info, path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat lock file %s: %w", path, err)
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

// reaperNameLockPaths prepares all compatibility barriers before their paths
// are sent to the external reaper. A partial set is never safe.
func reaperNameLockPaths(name string) ([]string, error) {
	resolved, err := resolveNameLocks(name)
	if err != nil {
		return nil, err
	}
	paths := resolved.ordered()
	if len(paths) != 3 {
		return nil, fmt.Errorf("name lock set has %d barriers, want 3", len(paths))
	}
	for _, path := range paths {
		f, err := openNameLockPath(path)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close prepared name lock %s: %w", path, err)
		}
	}
	return paths, nil
}

// nameLockIdentity returns a stable device/inode token for a prepared lock
// path. The reaper uses it to detect replacement while holding the lock.
func nameLockIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if err := checkLockFile(info, path); err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("lock file %s has no device/inode identity", path)
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func acquireNameLockFile(ctx context.Context, stage nameLockStage, path string, hooks *nameLockHooks) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hooks != nil && hooks.beforeOpen != nil {
		hooks.beforeOpen(stage, path)
	}
	f, err := openNameLockPath(path)
	if err != nil {
		return nil, err
	}
	if hooks != nil && hooks.afterOpen != nil {
		hooks.afterOpen(stage, path)
	}

	ticker := time.NewTicker(nameLockPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
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
	if err := checkOpenedNameLockFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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
}{byName: make(map[string]*processNameLock)}

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

func wrapNameLockError(stage nameLockStage, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("acquire %s name lock: %w", stage, err)
	}
	return fmt.Errorf("%w: acquire %s name lock: %w", ErrNameLockCompatibility, stage, err)
}

// lockName serializes name-addressed creates and generation-checked
// deletes across cooperating processes. All three namespaces are acquired
// in a fixed order; a caller that cannot establish one fails closed.
func lockName(ctx context.Context, name string) (func(), error) {
	return lockNameWithHooks(ctx, name, nil)
}

func lockNameWithHooks(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid container name %q", ErrNameLockCompatibility, name)
	}
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
		return nil, fmt.Errorf("%w: %w", ErrNameLockCompatibility, err)
	}
	stages := []nameLockStage{legacyNameLockStage, transitionalNameLockStage, stateNameLockStage}
	paths := resolved.ordered()
	acquired := make([]func(), 0, len(paths))
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	for i, path := range paths {
		unlock, err := acquireNameLockFile(ctx, stages[i], path, hooks)
		if err != nil {
			release()
			unlockProcess()
			return nil, wrapNameLockError(stages[i], err)
		}
		acquired = append(acquired, unlock)
	}
	if err := ctx.Err(); err != nil {
		release()
		unlockProcess()
		return nil, err
	}
	if hooks != nil && hooks.beforeCleanup != nil {
		hooks.beforeCleanup()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			unlockProcess()
		})
	}, nil
}

// validNameLockProtocolPath rejects paths that cannot safely be carried in
// the tab/newline-delimited reaper protocol.
func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
