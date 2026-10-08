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

// nameLockDir returns a private directory scoped to the current user. It
// deliberately does not use os.TempDir: separate processes commonly have
// different TMPDIR values but must still coordinate on the same host/user
// lock. Using /tmp directly ensures processes with different TMPDIR settings
// share the same lock namespace.
func nameLockDir() (string, error) {
	if override := os.Getenv("CONTAINERGO_LOCK_DIR"); override != "" {
		if err := ensurePrivateDir(override); err != nil {
			return "", err
		}
		return override, nil
	}
	base := "/tmp"
	if target, err := filepath.EvalSymlinks(base); err == nil {
		base = target
	}
	appDir := filepath.Join(base, fmt.Sprintf("container-go-%d", os.Geteuid()))
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

func reaperNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	path, err := nameLockPath(name)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, nameLockFilePerm)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// lockName serializes generation-checked, name-addressed deletes of one
// container name across processes on this host. Apple Container has no
// immutable container ID, so an inspect-then-delete by name is only
// safe for a non-empty, matching generation if no other process can
// delete and recreate the name in between. The current generation-checked
// name-addressed Terminate and failed-create cleanup paths take this lock
// first; an empty-generation legacy path and the external reaper do not.
// The current Apple Prune and PruneReuseGroup list-to-delete paths do not
// re-inspect candidates under this lock, so a replacement can occur
// between listing and deletion. The lock therefore protects only the
// ordinary delete/cleanup paths that explicitly use it; it does not close
// the prune list-to-delete window, the reaper window, or a direct
// `container` CLI race. #98 tracks the Apple prune list-to-delete race
// and cleanup revalidation. Closing the broader race would need an
// immutable ID or an atomic conditional delete from the backend, which
// Apple Container does not offer.
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
	path, err := nameLockPath(name)
	if err != nil {
		return nil, err
	}

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
