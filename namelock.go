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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	nameLockDirPerm         = 0o700
	nameLockFilePerm        = 0o600
	nameLockPoll            = 10 * time.Millisecond
	nameLockRetention       = 7 * 24 * time.Hour
	nameLockMaxFiles        = 256
	nameLockCleanupScan     = nameLockMaxFiles + 2
	nameLockCleanupDelete   = 32
	nameLockMaintenanceFile = ".maintenance.lock"
)

type nameLockStage string

const (
	legacyNameLockStage      nameLockStage = "legacy TMPDIR"
	cacheNameLockStage       nameLockStage = "transitional UserCacheDir"
	stateNameLockStage       nameLockStage = "durable state"
	maintenanceNameLockStage nameLockStage = "lock maintenance"
)

// nameLockHooks exists so tests can place a substitution at each side of
// the final-component open without mutating process-global test state.
type nameLockHooks struct {
	beforeOpen func(nameLockStage, string)
	afterOpen  func(nameLockStage, string)
}

// nameLockPath returns the durable, per-user state path for name. The
// digest keeps arbitrary container names out of the filesystem path and
// avoids both path traversal and filename-length surprises.
func nameLockPath(name string) (string, error) {
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

// nameLockDir returns a private directory below the durable per-user
// state root. XDG_STATE_HOME is honored when absolute; otherwise the
// platform's user state directory is used. There is deliberately no
// temporary-directory fallback because lock-file retention is part of the
// coordination protocol.
func nameLockDir() (string, error) {
	stateDir, err := userStateDir()
	if err != nil {
		return "", err
	}
	appDir, err := ensurePrivateDir(filepath.Join(stateDir, "container-go"))
	if err != nil {
		return "", err
	}
	lockDir, err := ensurePrivateDir(filepath.Join(appDir, "locks"))
	if err != nil {
		return "", err
	}
	return lockDir, nil
}

func userStateDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if filepath.IsAbs(stateHome) {
		return prepareUserBase(stateHome, "user state directory")
	}
	home, err := currentUserHome()
	if err != nil {
		return "", fmt.Errorf("find durable user state directory: %w", err)
	}
	return prepareUserBase(defaultStateDir(home, stateHome), "user state directory")
}

func defaultStateDir(home, stateHome string) string {
	if filepath.IsAbs(stateHome) {
		return filepath.Clean(stateHome)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".local", "state")
}

// currentUserHome prefers the account database over HOME so unrelated
// process environments cannot silently select different lock namespaces.
// The HOME-derived value is only a fallback and is accepted only after
// the same ownership and directory-safety checks.
func currentUserHome() (string, error) {
	var errs []error
	if current, err := user.Current(); err != nil {
		errs = append(errs, fmt.Errorf("look up current user: %w", err))
	} else if home, err := existingUserDir(current.HomeDir); err != nil {
		errs = append(errs, fmt.Errorf("use account home %q: %w", current.HomeDir, err))
	} else {
		return home, nil
	}
	if home, err := os.UserHomeDir(); err != nil {
		errs = append(errs, fmt.Errorf("use HOME fallback: %w", err))
	} else if resolved, err := existingUserDir(home); err != nil {
		errs = append(errs, fmt.Errorf("use HOME fallback %q: %w", home, err))
	} else {
		return resolved, nil
	}
	return "", errors.Join(errs...)
}

func existingUserDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if err := ensureSecureDirPath(resolved, true); err != nil {
		return "", err
	}
	return resolved, nil
}

// transitionalNameLockPath is the namespace introduced by the first
// hardened revision. It remains a mandatory migration barrier until old
// binaries can no longer coexist with this one.
func transitionalNameLockPath(name string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find transitional user cache directory: %w", err)
	}
	cacheDir, err = prepareUserBase(cacheDir, "transitional user cache directory")
	if err != nil {
		return "", err
	}
	appDir, err := ensurePrivateDir(filepath.Join(cacheDir, "container-go"))
	if err != nil {
		return "", err
	}
	lockDir, err := ensurePrivateDir(filepath.Join(appDir, "locks"))
	if err != nil {
		return "", err
	}
	return filepath.Join(lockDir, nameLockFileName(name)), nil
}

func legacyNameLockPath(name string) (string, error) {
	tempDir := os.TempDir()
	// Resolve safe system symlinks such as /var -> /private/var. The old
	// binary opens the alias, while this revision opens the same final
	// directory inode.
	resolved, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		return "", fmt.Errorf("resolve legacy temporary directory %s: %w", tempDir, err)
	}
	if err := ensureSecureDirPath(resolved, false); err != nil {
		return "", fmt.Errorf("legacy temporary directory %s is not user-safe: %w", resolved, err)
	}
	return filepath.Join(resolved, "containergo-"+name+".lock"), nil
}

// prepareUserBase resolves all existing path components, creates missing
// private components, and returns the canonical path. Final components
// must be owned by the current user and inaccessible for group/other
// writes; intermediate sticky directories such as /tmp are safe because
// their sticky rule prevents another user from replacing our entry.
func prepareUserBase(path, kind string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s %s is not absolute", kind, path)
	}
	canonical, err := pathForCreate(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", kind, path, err)
	}
	if err := ensureSecureDirPath(canonical, true); err != nil {
		return "", fmt.Errorf("prepare %s %s: %w", kind, path, err)
	}
	return canonical, nil
}

func pathForCreate(path string) (string, error) {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %s is a symbolic link", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var missing []string
	current := path
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

func ensureSecureDirPath(path string, userOwnedFinal bool) error {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if parent != path {
		if err := ensureSecureDirPath(parent, false); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, nameLockDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open directory %s without following links: %w", path, err)
	}
	info, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil {
		return fmt.Errorf("stat directory %s: %w", path, statErr)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat lock directory %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock directory %s is not a directory", path)
	}
	if !os.SameFile(info, pathInfo) {
		return fmt.Errorf("lock directory %s changed while opening", path)
	}
	if err := checkNamespaceOwner(info, path); err != nil {
		return err
	}
	writable := info.Mode().Perm() & 0o022
	if writable != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("lock directory %s is writable by group or other (%04o)", path, info.Mode().Perm())
	}
	if userOwnedFinal {
		if err := checkLockOwner(info, "lock directory"); err != nil {
			return err
		}
		if writable != 0 {
			return fmt.Errorf("lock directory %s is writable by group or other (%04o)", path, info.Mode().Perm())
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("lock directory %s has special mode bits", path)
		}
	}
	return nil
}

func ensurePrivateDir(path string) (string, error) {
	parent, err := prepareUserBase(filepath.Dir(path), "private lock parent")
	if err != nil {
		return "", err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if err := ensureSecureDirPath(path, true); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat private lock directory %s: %w", path, err)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		return "", fmt.Errorf("lock directory %s has permissions %04o, want %04o", path, info.Mode().Perm(), nameLockDirPerm)
	}
	return path, nil
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

// lockName serializes generation-checked, name-addressed deletes across
// cooperating processes and library revisions on this host. Every new
// caller takes three barriers in a fixed order: the parent revision's
// TMPDIR flock, the UserCacheDir flock introduced by the first hardened
// revision, and the durable state flock. An old caller takes its one
// historical barrier, so no old/new pair can enter the critical section
// together. Failure to establish any historical barrier is a fail-closed
// compatibility error.
func lockName(ctx context.Context, name string) (func(), error) {
	return lockNameWithHooks(ctx, name, nil)
}

func lockNameWithHooks(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid container name %q", ErrNameLockCompatibility, name)
	}

	// Resolve all paths before taking a barrier. In particular, a missing
	// UserCacheDir is surfaced instead of silently entering a critical
	// section that an older hardened process could also enter.
	legacyPath, err := legacyNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve legacy lock: %w", ErrNameLockCompatibility, err)
	}
	cachePath, err := transitionalNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve transitional lock: %w", ErrNameLockCompatibility, err)
	}
	statePath, err := nameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve durable lock: %w", ErrNameLockCompatibility, err)
	}

	var acquired []func()
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	acquire := func(stage nameLockStage, path string, touch bool) error {
		unlock, err := acquireLockFile(ctx, stage, path, touch, hooks)
		if err != nil {
			return fmt.Errorf("%w: acquire %s lock: %w", ErrNameLockCompatibility, stage, err)
		}
		acquired = append(acquired, unlock)
		return nil
	}

	// This order is a compatibility invariant. Do not reorder these calls.
	if err := acquire(legacyNameLockStage, legacyPath, false); err != nil {
		release()
		return nil, err
	}
	if err := acquire(cacheNameLockStage, cachePath, false); err != nil {
		release()
		return nil, err
	}

	stateDir := filepath.Dir(statePath)
	maintenancePath := filepath.Join(stateDir, nameLockMaintenanceFile)
	maintenanceUnlock, err := acquireLockFile(ctx, maintenanceNameLockStage, maintenancePath, false, nil)
	if err != nil {
		release()
		return nil, fmt.Errorf("%w: acquire lock maintenance: %w", ErrNameLockCompatibility, err)
	}
	if err := acquire(stateNameLockStage, statePath, true); err != nil {
		maintenanceUnlock()
		release()
		return nil, err
	}
	maintenanceUnlock()

	if err := cleanupNameLockFiles(ctx, stateDir, statePath, time.Now()); err != nil {
		release()
		return nil, fmt.Errorf("%w: lock maintenance: %w", ErrNameLockCompatibility, err)
	}
	return release, nil
}

func acquireLockFile(ctx context.Context, stage nameLockStage, path string, touch bool, hooks *nameLockHooks) (func(), error) {
	f, err := openLockFile(path, true, stage, hooks)
	if err != nil {
		return nil, err
	}
	if err := flockWithContext(ctx, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		closeFileLock(f)
		return nil, err
	}
	if touch {
		now := time.Now()
		times := []syscall.Timeval{syscall.NsecToTimeval(now.UnixNano())}
		_ = syscall.Futimes(int(f.Fd()), times)
	}
	return closeFileLock(f), nil
}

func openLockFile(path string, create bool, stage nameLockStage, hooks *nameLockHooks) (*os.File, error) {
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
		}
		if errors.Is(err, fs.ErrExist) {
			// Another new process won creation, or a pre-existing file was
			// validated above. Never follow a symlink installed in between.
			f, err = os.OpenFile(path, flags, 0)
		}
	} else {
		f, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock file %s without following links: %w", path, err)
	}
	if created {
		// Restore the intended mode even if a process has an unusually
		// restrictive umask; this file was created with O_EXCL by us.
		if err := f.Chmod(nameLockFilePerm); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("set lock file permissions %s: %w", path, err)
		}
	}
	if hooks != nil && hooks.afterOpen != nil {
		hooks.afterOpen(stage, path)
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

func closeFileLock(f *os.File) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
}

// cleanupNameLockFiles performs a bounded opportunistic sweep. It never
// waits for a name lock: a busy candidate is live and is skipped. The
// namespace maintenance lock excludes the open-then-flock window used by
// all new callers, so a candidate cannot be unlinked between those steps.
// The scan is capped at nameLockMaxFiles plus the maintenance entry; if
// that cap is exceeded, only unlocked candidates are removed.
func cleanupNameLockFiles(ctx context.Context, dir, keep string, now time.Time) error {
	return cleanupNameLockFilesAt(ctx, dir, keep, now, nameLockCleanupScan, nameLockCleanupDelete)
}

func cleanupNameLockFilesAt(ctx context.Context, dir, keep string, now time.Time, scanLimit, deleteLimit int) error {
	maintenanceUnlock, err := acquireLockFile(ctx, maintenanceNameLockStage, filepath.Join(dir, nameLockMaintenanceFile), false, nil)
	if err != nil {
		return err
	}
	defer maintenanceUnlock()

	df, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open lock directory %s: %w", dir, err)
	}
	entries, readErr := df.ReadDir(scanLimit)
	closeErr := df.Close()
	if readErr != nil {
		return fmt.Errorf("read lock directory %s: %w", dir, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock directory %s: %w", dir, closeErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	validNames := 0
	for _, entry := range entries {
		if isNameLockFile(entry.Name()) {
			validNames++
		}
	}
	overLimit := len(entries) >= scanLimit || validNames > nameLockMaxFiles

	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if removed >= deleteLimit {
			break
		}
		name := entry.Name()
		if name == filepath.Base(keep) || name == nameLockMaintenanceFile || !isNameLockFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := openLockFile(path, false, stateNameLockStage, nil)
		if err != nil {
			// Invalid, replaced, or concurrently removed entries are not
			// removed. Retention must never turn a metadata conflict into
			// deletion of an unknown inode.
			continue
		}
		unlock := closeFileLock(f)
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(flockErr, syscall.EWOULDBLOCK) || errors.Is(flockErr, syscall.EAGAIN) {
			unlock()
			continue
		}
		if flockErr != nil {
			unlock()
			return fmt.Errorf("probe stale lock file %s: %w", path, flockErr)
		}
		if err := checkOpenedLockFile(f, path); err != nil {
			unlock()
			continue
		}
		info, err := f.Stat()
		if err != nil || (!overLimit && now.Sub(info.ModTime()) < nameLockRetention) {
			unlock()
			continue
		}
		// Release the probe before unlinking. The maintenance lock keeps
		// another new caller from opening this path, so the inode is not
		// live at the instant it is removed.
		unlock()
		current, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("recheck stale lock file %s: %w", path, err)
		}
		if !os.SameFile(info, current) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale lock file %s: %w", path, err)
		}
		removed++
	}
	return nil
}

func isNameLockFile(name string) bool {
	if !strings.HasSuffix(name, ".lock") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimSuffix(name, ".lock"))
	return err == nil && len(decoded) == sha256.Size
}
