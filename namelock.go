//go:build !windows

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	nameLockDirPerm  = 0o700
	nameLockFilePerm = 0o600
	nameLockPoll     = 10 * time.Millisecond
)

// nameLockPath returns the persistent, user-scoped path for name. The
// digest keeps arbitrary container names out of the filesystem path and
// avoids both path traversal and filename-length surprises.
func nameLockPath(name string) (string, error) {
	dir, err := nameLockDir()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), nil
}

// nameLockDir returns a private directory below the user's cache
// directory. It deliberately does not use os.TempDir: separate processes
// commonly have different TMPDIR values but must still coordinate on the
// same host/user lock.
func nameLockDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	appDir := filepath.Join(cacheDir, "container-go")
	lockDir := filepath.Join(appDir, "locks")
	for _, dir := range []string{appDir, lockDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return "", err
		}
	}
	return lockDir, nil
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
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat open lock file %s: %w", path, err)
	}
	if err := checkLockFile(info, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openNameLock(name string) (*os.File, error) {
	path, err := nameLockPath(name)
	if err != nil {
		return nil, err
	}
	return openNameLockPath(path)
}

// reaperNameLockPath prepares the lock file before the shell reaper can
// receive its path. The reaper protocol is fail-closed: if this succeeds,
// the script can use the same inode that lockName uses; if it fails, no
// name-addressed reaper entry is registered.
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
		return "", err
	}
	return path, nil
}

// lockName serializes generation-checked, name-addressed creates and
// deletes of one container name across processes on this host. Apple
// Container has no immutable container ID, so an inspect-then-delete by
// name is only safe if no other process can mutate the name in between;
// the guarded create and delete paths in this library take this lock
// first. That covers cooperating processes using this guarded protocol
// only. A direct `container` CLI invocation, an unguarded library
// operation, or another external actor does not take the lock and can
// change the state, labels, generation, or name target after inspect
// and before delete. Closing that would need an immutable ID or an
// atomic conditional delete from the backend, which Apple Container does
// not offer.
//
// Lock files are intentionally persistent. flock locks are released by
// the kernel when a process exits, so a leftover file is stale but
// reusable rather than a reason to unlink it; unlinking would permit a
// second process to lock a different inode while the first still holds
// the old one.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openNameLock(name)
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
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
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
}

// nameLockProtocolPath is kept small and explicit because the shell reaper
// receives the path as data. Rejecting control characters prevents a path
// from changing the tab/newline-delimited reaper protocol.
func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
