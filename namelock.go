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

// nameLockPath returns a persistent, user-scoped path for a container name.
// Hashing the name keeps arbitrary names out of the filesystem path and
// makes the path stable across processes with different TMPDIR values.
func nameLockPath(name string) (string, error) {
	dir, err := nameLockDir()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), nil
}

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

// ensurePrivateDir creates the lock namespace and refuses to use a path
// that could have been replaced by another user. The directory is kept
// persistent: unlinking a lock file would let two processes lock different
// inodes for the same name.
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

// reaperNameLockPath prepares the exact stable file that the shell reaper
// will lock. Preparation is done in Go so the reaper never has to guess a
// path or create a different namespace.
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

// lockName serializes generation-checked, name-addressed operations across
// cooperating processes. Apple Container has no immutable container ID, so
// an inspect-then-delete by name is safe only while the same stable lock is
// held across the complete critical section. Direct backend CLI calls and
// other implementations do not participate in this protocol.
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
}

// nameLockProtocolPath is passed through the reaper's line protocol. Reject
// control characters so a path cannot alter its framing or shell parsing.
func validNameLockProtocolPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\n\r\t")
}
