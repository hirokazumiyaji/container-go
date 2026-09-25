//go:build !windows

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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

type nameLockStage string

const (
	legacyNameLockStage nameLockStage = "legacy TMPDIR"
	cacheNameLockStage  nameLockStage = "transitional UserCacheDir"
	stateNameLockStage  nameLockStage = "durable account state"
)

// nameLockHooks is a narrow test seam for races between path validation and
// open. Production callers pass nil.
type nameLockHooks struct {
	beforeOpen func(nameLockStage, string)
	afterOpen  func(nameLockStage, string)
}

// nameLockStateRootOverride is a package-test hook. Production code leaves
// it empty and derives the namespace from the account database rather than
// mutable process environment variables.
var nameLockStateRootOverride string

func validLockName(name string) bool {
	return nameRE.MatchString(name)
}

func nameLockFileName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:]) + ".lock"
}

// resolveTrustedBase resolves aliases in an established platform/home/cache
// base while retaining missing trailing components. The private namespace
// appended by the callers is still walked strictly by privateDirPath, so a
// symlink introduced inside container-go/locks is never silently followed.
func resolveTrustedBase(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("trusted lock base %q is not absolute", path)
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
		if !errors.Is(err, fs.ErrNotExist) {
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

// nameLockPath returns the durable, account-scoped coordination path. The
// file name is hashed so arbitrary container names cannot alter the path.
func nameLockPath(name string) (string, error) {
	if !validLockName(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	dir, err := durableNameLockDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, nameLockFileName(name)), nil
}

func legacyNameLockPath(name string) (string, error) {
	if !validLockName(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	// This is the exact path used by the pre-cache revision. Resolve a
	// system symlink such as /var -> /private/var so a new process and an
	// old process open the same directory inode.
	tempDir, err := resolveTrustedBase(os.TempDir())
	if err != nil {
		return "", fmt.Errorf("resolve legacy temporary directory: %w", err)
	}
	return filepath.Join(tempDir, "containergo-"+name+".lock"), nil
}

func transitionalNameLockPath(name string) (string, error) {
	if !validLockName(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("transitional user cache directory: %w", err)
	}
	cacheDir, err = resolveTrustedBase(cacheDir)
	if err != nil {
		return "", fmt.Errorf("resolve transitional user cache directory: %w", err)
	}
	lockDir, err := privateDirPath(filepath.Join(cacheDir, "container-go", "locks"))
	if err != nil {
		return "", fmt.Errorf("transitional user cache directory: %w", err)
	}
	return filepath.Join(lockDir, nameLockFileName(name)), nil
}

func durableNameLockDir() (string, error) {
	base := nameLockStateRootOverride
	if base == "" {
		current, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("find account home: %w", err)
		}
		base = current.HomeDir
	}
	base, err := resolveTrustedBase(base)
	if err != nil {
		return "", fmt.Errorf("resolve durable account base: %w", err)
	}
	if runtime.GOOS == "darwin" {
		base = filepath.Join(base, "Library", "Application Support")
	} else {
		base = filepath.Join(base, ".local", "state")
	}
	appDir, err := privateDirPath(filepath.Join(base, "container-go"))
	if err != nil {
		return "", err
	}
	return privateDirPath(filepath.Join(appDir, "locks"))
}

// nameLockDir is retained as the historical helper name; it now returns
// the durable account-scoped namespace.
//
//nolint:unused // retained as the historical namespace helper
func nameLockDir() (string, error) {
	return durableNameLockDir()
}

// ensurePrivateDir retains the original error-only helper contract.
//
//nolint:unused // retained as the historical error-only directory helper
func ensurePrivateDir(dir string) error {
	_, err := privateDirPath(dir)
	return err
}

func privateDirPath(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("lock directory %s is not absolute", dir)
	}
	dir = filepath.Clean(dir)
	if err := ensureDirectoryParents(dir); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("stat lock directory %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("lock directory %s is not a directory", dir)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		return "", fmt.Errorf("lock directory %s has permissions %04o, want %04o", dir, info.Mode().Perm(), nameLockDirPerm)
	}
	if err := checkLockOwner(info, "lock directory"); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureDirectoryParents creates missing path components one at a time and
// rejects symlinked or group/other-writable components. A world-writable
// sticky directory such as /tmp is safe because its sticky rule prevents a
// different account from replacing an entry owned by this user.
func ensureDirectoryParents(path string) error {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	current := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimLeft(rest, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, nameLockDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("create lock directory %s: %w", current, err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("stat lock directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("lock directory %s is not a directory", current)
		}
		if current != filepath.Clean(path) {
			if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("lock directory %s is writable by group or other (%04o)", current, info.Mode().Perm())
			}
			if err := checkNamespaceOwner(info, current); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkNamespaceOwner(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("lock directory %s ownership is unavailable", path)
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid && stat.Uid != 0 {
		return fmt.Errorf("lock directory %s is owned by uid %d, want uid %d or root", path, stat.Uid, uid)
	}
	return nil
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

// openNameLockPath opens one stable lock inode and rechecks the pathname
// after open. O_NOFOLLOW closes the pre-open symlink race; SameFile closes
// the replacement race between open and the first flock attempt.
func openNameLockPath(path string) (*os.File, error) {
	return openLockFile(path, true)
}

func openLockFile(path string, create bool) (*os.File, error) {
	return openLockFileWithHooks(path, create, nil, "")
}

func openLockFileWithHooks(path string, create bool, hooks *nameLockHooks, stage nameLockStage) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if err := checkLockFile(info, path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat lock file %s: %w", path, err)
	} else if !create {
		return nil, err
	}

	if hooks != nil && hooks.beforeOpen != nil {
		hooks.beforeOpen(stage, path)
	}
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	var f *os.File
	var err error
	created := false
	if create {
		f, err = os.OpenFile(path, flags|os.O_CREATE|syscall.O_EXCL, nameLockFilePerm)
		if err == nil {
			created = true
		} else if errors.Is(err, fs.ErrExist) {
			f, err = os.OpenFile(path, flags, 0)
		}
	} else {
		f, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock file %s without following links: %w", path, err)
	}
	if hooks != nil && hooks.afterOpen != nil {
		hooks.afterOpen(stage, path)
	}
	if created {
		if err := f.Chmod(nameLockFilePerm); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("set lock file permissions %s: %w", path, err)
		}
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkOpenedLockFile(f *os.File, path string) error {
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

//nolint:unused // retained for package callers opening the durable lock directly
func openNameLock(name string) (*os.File, error) {
	path, err := nameLockPath(name)
	if err != nil {
		return nil, err
	}
	return openNameLockPath(path)
}

// reaperNameLockPaths prepares every barrier used by lockName for the
// shell reaper. The values are returned in the same order as lockName.
//
//nolint:unused // retained for callers that only need paths
func reaperNameLockPaths(name string) ([]string, error) {
	paths, _, err := reaperNameLockMetadata(name)
	return paths, err
}

func reaperLockIdentity(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("lock file inode metadata is unavailable")
	}
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, stat.Uid), nil
}

func reaperNameLockMetadata(name string) ([]string, []string, error) {
	paths := make([]string, 0, 3)
	identities := make([]string, 0, 3)
	for _, resolve := range []func(string) (string, error){
		legacyNameLockPath,
		transitionalNameLockPath,
		nameLockPath,
	} {
		path, err := resolve(name)
		if err != nil {
			return nil, nil, err
		}
		f, err := openNameLockPath(path)
		if err != nil {
			return nil, nil, err
		}
		identity, identityErr := reaperLockIdentity(f)
		closeErr := f.Close()
		if identityErr != nil {
			return nil, nil, identityErr
		}
		if closeErr != nil {
			return nil, nil, fmt.Errorf("close prepared reaper lock file %s: %w", path, closeErr)
		}
		paths = append(paths, path)
		identities = append(identities, identity)
	}
	return paths, identities, nil
}

// reaperNameLockPath retains the historical single-path helper and returns
// the durable path, the final barrier in reaperNameLockPaths.
//
//nolint:unused // retained for callers using the original single-path helper
func reaperNameLockPath(name string) (string, error) {
	path, err := nameLockPath(name)
	if err != nil {
		return "", err
	}
	f, err := openNameLockPath(path)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close prepared reaper lock file %s: %w", path, err)
	}
	return path, nil
}

func flockWithContext(ctx context.Context, f *os.File) error {
	ticker := time.NewTicker(nameLockPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// acquireLockFile accepts both the original (path, create) form and the
// migration-test form (stage, path, create, hooks). Keeping the small
// compatibility shim avoids breaking in-package integrations while the
// typed implementation remains available below.
func acquireLockFile(ctx context.Context, args ...interface{}) (func(), error) {
	var stage nameLockStage
	var path string
	var create bool
	var hooks *nameLockHooks
	switch len(args) {
	case 2:
		path, _ = args[0].(string)
		create, _ = args[1].(bool)
	case 4:
		stage, _ = args[0].(nameLockStage)
		path, _ = args[1].(string)
		create, _ = args[2].(bool)
		hooks, _ = args[3].(*nameLockHooks)
	case 5:
		stage, _ = args[0].(nameLockStage)
		path, _ = args[1].(string)
		create, _ = args[2].(bool)
		hooks, _ = args[3].(*nameLockHooks)
	default:
		return nil, fmt.Errorf("invalid lock acquisition arguments")
	}
	return acquireLockFileWithHooks(ctx, path, create, hooks, stage)
}

func acquireLockFileWithHooks(ctx context.Context, path string, create bool, hooks *nameLockHooks, stage nameLockStage) (func(), error) {
	f, err := openLockFileWithHooks(path, create, hooks, stage)
	if err != nil {
		return nil, err
	}
	if err := flockWithContext(ctx, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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

// lockName serializes name-addressed operations across all cooperating
// revisions. The order is part of the migration protocol: the original
// TMPDIR barrier, the first hardened UserCacheDir barrier, then the durable
// account-scoped barrier. Old processes take a prefix of this order, so an
// old/new pair cannot enter the same critical section simultaneously.
func lockName(ctx context.Context, name string) (func(), error) {
	return lockNameWithHooks(ctx, name, nil)
}

func lockNameWithHooks(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validLockName(name) {
		return nil, fmt.Errorf("%w: invalid container name %q", ErrNameLockCompatibility, name)
	}
	legacyPath, err := legacyNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve legacy lock: %w", ErrNameLockCompatibility, err)
	}
	cachePath, err := transitionalNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve transitional lock: %w", ErrNameLockCompatibility, err)
	}
	durablePath, err := nameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve durable lock: %w", ErrNameLockCompatibility, err)
	}

	var acquired []func()
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	acquire := func(stage nameLockStage, path string) error {
		unlock, err := acquireLockFileWithHooks(ctx, path, true, hooks, stage)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("acquire %s lock: %w", stage, err)
			}
			return fmt.Errorf("%w: acquire %s lock: %w", ErrNameLockCompatibility, stage, err)
		}
		acquired = append(acquired, unlock)
		return nil
	}
	if err := acquire(legacyNameLockStage, legacyPath); err != nil {
		release()
		return nil, err
	}
	if err := acquire(cacheNameLockStage, cachePath); err != nil {
		release()
		return nil, err
	}
	if err := acquire(stateNameLockStage, durablePath); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// nameLockProtocolPath is passed through the reaper's line protocol. Reject
// control characters so a path cannot alter its framing or shell parsing.
func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t;")
}
