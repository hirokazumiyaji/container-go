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
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	nameLockDirPerm  = 0o700
	nameLockFilePerm = 0o600
	nameLockPoll     = 10 * time.Millisecond
)

// nameLockBarriers lists the lock barriers of one container name in the
// fixed acquisition order every cooperating path and the shell reaper use.
const (
	nameLockStageLegacy       = "legacy"
	nameLockStageTransitional = "transitional"
	nameLockStageDurable      = "durable"
	nameLockBarrierCount      = 3
)

func privateOwned(info os.FileInfo) bool {
	if info == nil || (!info.Mode().IsRegular() && !info.IsDir()) {
		return false
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid == uint32(os.Getuid())
	}
	return true
}

func nameLockFileName(name string) string {
	// The digest keeps arbitrary names out of the filesystem path.
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:]) + ".lock"
}

// resolvedNameLocks is the barrier set of one container name.
type resolvedNameLocks struct {
	legacy       string
	transitional string
	durable      string
}

// ordered returns the barriers in the fixed order they must be acquired:
// legacy first, then transitional, then the durable account barrier. A
// fixed order keeps the barriers deadlock-free even when cooperating
// processes disagree about which environment-derived root is current.
func (l resolvedNameLocks) ordered() []string {
	return []string{l.legacy, l.transitional, l.durable}
}

func nameLockStages() []string {
	return []string{nameLockStageLegacy, nameLockStageTransitional, nameLockStageDurable}
}

// legacyNameLockPath is the original TMPDIR namespace. It is kept as the
// first barrier so binaries that predate the migration still exclude a
// new process.
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

// transitionalNameLockPath is the environment-selected UserCacheDir
// namespace this library used before the durable account namespace. It is
// retained as a compatibility barrier: processes launched with a different
// HOME or XDG_CACHE_HOME still exclude each other through it, and a
// process that only knows the old path still excludes this one.
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

// nameLockPath returns the durable, account-scoped barrier for name. It
// deliberately ignores HOME, XDG_CACHE_HOME, and XDG_STATE_HOME and
// includes the account uid, so cooperating processes select the same
// barrier even when they were launched with a different environment.
func nameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	stateDir, err := accountStateDir()
	if err != nil {
		return "", err
	}
	appDir := filepath.Join(stateDir, "container-go", strconv.Itoa(os.Getuid()))
	lockDir := filepath.Join(appDir, "locks")
	for _, dir := range []string{appDir, lockDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return "", err
		}
	}
	return filepath.Join(lockDir, nameLockFileName(name)), nil
}

// accountStateDir is the account's own durable state location, taken from
// the account database rather than the process environment.
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

func resolveNameLocks(name string) (resolvedNameLocks, error) {
	legacy, err := legacyNameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve %s lock: %w", nameLockStageLegacy, err)
	}
	transitional, err := transitionalNameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve %s lock: %w", nameLockStageTransitional, err)
	}
	durable, err := nameLockPath(name)
	if err != nil {
		return resolvedNameLocks{}, fmt.Errorf("resolve %s lock: %w", nameLockStageDurable, err)
	}
	return resolvedNameLocks{legacy: legacy, transitional: transitional, durable: durable}, nil
}

// ensurePrivateDir creates dir if needed and refuses a path another user
// could have substituted. The directory is kept persistent: removing it
// would let a new inode bypass an existing flock.
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
	if !privateOwned(info) {
		return fmt.Errorf("lock directory %s is not private to this account", dir)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		return fmt.Errorf("lock directory %s has permissions %04o, want %04o", dir, info.Mode().Perm(), nameLockDirPerm)
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
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("lock file %s is accessible to other users", path)
	}
	return nil
}

// openNameLockFile opens a barrier inode and verifies it is the private
// regular file that is still installed at that path.
func openNameLockFile(path string) (*os.File, error) {
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
	if !privateOwned(opened) {
		return fmt.Errorf("lock file %s is not private to this account", path)
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

func acquireNameLockFile(ctx context.Context, path string) (func(), error) {
	f, err := openNameLockFile(path)
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
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
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
	// Recheck after flock too: a barrier replaced while this caller waits
	// must not let it enter a critical section through a foreign inode.
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

// reaperNameLockMetadata prepares every barrier of name and returns the
// paths plus their device:inode:uid identity. The reaper protocol is
// fail-closed: the child refuses to delete when a barrier no longer has
// the identity the library registered.
func reaperNameLockMetadata(name string) ([]string, []string, error) {
	resolved, err := resolveNameLocks(name)
	if err != nil {
		return nil, nil, err
	}
	paths := resolved.ordered()
	identities := make([]string, 0, len(paths))
	for _, path := range paths {
		f, err := openNameLockFile(path)
		if err != nil {
			return nil, nil, err
		}
		identity, identityErr := reaperLockIdentity(f)
		closeErr := f.Close()
		if identityErr != nil {
			return nil, nil, identityErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		identities = append(identities, identity)
	}
	return paths, identities, nil
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

// lockName serializes generation-checked, name-addressed creates and
// deletes of one container name across cooperating processes on this
// host. Apple Container has no immutable container ID, so an
// inspect-then-delete by name is only safe while every cooperating path
// that can mutate that name holds these barriers.
//
// All three barriers are acquired in the fixed legacy-to-durable order and
// released in reverse. Holding the legacy and transitional barriers keeps
// a process built against an older lock location mutually exclusive with
// this one; the durable, account-scoped barrier is the one every
// cooperating process agrees on even when HOME, XDG_CACHE_HOME, or
// TMPDIR differ. Lock files are intentionally persistent: flock is
// released by the kernel when a process exits, so a leftover file is
// stale but reusable, and unlinking it would let a second process lock a
// different inode while the first still holds the old one.
func lockName(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, queryTimeout)
		defer cancel()
	}
	resolved, err := resolveNameLocks(name)
	if err != nil {
		return nil, err
	}
	paths := resolved.ordered()
	stages := nameLockStages()
	acquired := make([]func(), 0, len(paths))
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	for i, path := range paths {
		unlock, err := acquireNameLockFile(ctx, path)
		if err != nil {
			release()
			return nil, fmt.Errorf("lock name %s: acquire %s barrier: %w", name, stages[i], err)
		}
		acquired = append(acquired, unlock)
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(release)
	}, nil
}
