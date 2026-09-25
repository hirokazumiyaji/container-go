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

const nameLockPoll = 10 * time.Millisecond

// nameLockPath returns a stable, user-scoped path for name. The digest
// keeps arbitrary names out of the filesystem path and, unlike TempDir,
// gives cooperating processes the same lock even when they have different
// TMPDIR values.
func reaperNameLockPath(name string) (string, error) {
	path, err := nameLockPath(name)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("prepare reaper lock file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close reaper lock file %s: %w", path, err)
	}
	return path, nil
}

func nameLockPath(name string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	dir := filepath.Join(cacheDir, "container-go", "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create lock directory %s: %w", dir, err)
	}
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".lock"), nil
}

// lockName serializes name-addressed creates and generation-checked
// deletes of one container name across cooperating processes on this
// host. Apple Container has no immutable container ID, so an inspect-
// then-delete (or create) by name is only safe while all library paths
// that can mutate that name hold this lock. Direct CLI calls and other
// implementations do not participate in the protocol.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := nameLockPath(name)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
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
