//go:build !windows

package container

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
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

	// nameLockBucketCount is the fixed number of lock files in the lock
	// directory. The first two digest bytes, reduced modulo this count,
	// select a bucket, so a host that creates an unbounded number of
	// container names still never holds more than this many lock inodes.
	nameLockBucketCount = 256
)

// nameLockPath returns the persistent, user-scoped path for name. The name
// selects one of nameLockBucketCount fixed buckets: container names never
// become filenames, so path traversal and filename-length surprises are
// impossible, and a name-addressed operation never costs a new inode. A
// bucket collision only serializes names that did not need to run
// concurrently; it cannot weaken the exclusion a name's lock provides.
func nameLockPath(name string) (string, error) {
	dir, err := nameLockDir()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(name))
	bucket := int(binary.BigEndian.Uint16(digest[:2])) % nameLockBucketCount
	return filepath.Join(dir, fmt.Sprintf("bucket-%03d.lock", bucket)), nil
}

// nameLockDir returns a private directory scoped to the current user. The
// per-user cache directory keeps the lock outside world-writable temporary
// directories, where another local user could pre-create a fixed path and
// permanently deny every locked operation. It is also independent of
// TMPDIR, so processes that see different temporary directories still
// coordinate. The cache base and the containergo directory are validated by
// the same helper copy staging uses, so both callers reject a substituted,
// foreign, or writable path. Processes that do not share a cache directory
// (for example a different HOME or XDG_CACHE_HOME) must set
// CONTAINERGO_LOCK_DIR to the same path to share the lock namespace.
func nameLockDir() (string, error) {
	if override := os.Getenv("CONTAINERGO_LOCK_DIR"); override != "" {
		dir, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve lock directory %s: %w", override, err)
		}
		if err := ensurePrivateDir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	appDir, err := ensureContainergoCacheRoot(nil)
	if err != nil {
		return "", lockDirHint(err)
	}
	lockDir := filepath.Join(appDir, "locks")
	if err := ensurePrivateCacheDir(lockDir, nil); err != nil {
		return "", lockDirHint(err)
	}
	return lockDir, nil
}

// lockDirHint tells the operator how to recover when the resolved cache
// directory cannot host the private lock directory.
func lockDirHint(err error) error {
	return fmt.Errorf("%w; set CONTAINERGO_LOCK_DIR to a private directory to override", err)
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

// openLockFile opens path for locking. O_NOFOLLOW and O_NONBLOCK keep the
// open itself from following a substituted symlink or blocking on a FIFO; the
// file is then checked by openVerifiedLockFile. os.OpenFile applies
// O_CLOEXEC to every descriptor it returns.
func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, nameLockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	return f, nil
}

// openVerifiedLockFile opens path and validates that the file is a private
// regular file owned by this user. The pre-open check refuses an existing
// unsafe entry before it is opened at all; the post-open fstat closes the
// race with a replacement between the check and open.
func openVerifiedLockFile(path string) (*os.File, error) {
	if info, statErr := os.Lstat(path); statErr == nil {
		if err := checkLockFile(info, path); err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat lock file %s: %w", path, statErr)
	}
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
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

// reaperNameLockPath returns the lock path the reaper will pass to flock and
// makes sure the file exists with the same validation lockName applies. The
// reaper script only checks for a symlink, so an unwritable, non-regular, or
// foreign lock file must be rejected here.
func reaperNameLockPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	path, err := nameLockPath(name)
	if err != nil {
		return "", err
	}
	f, err := openVerifiedLockFile(path)
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
// the old one. nameLockPath maps every name onto a bounded set of bucket
// files, so this persistence cannot accumulate one inode per container
// name.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := nameLockPath(name)
	if err != nil {
		return nil, err
	}
	f, err := openVerifiedLockFile(path)
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
		err := flockFile(int(f.Fd()), lockExclusiveNonBlocking)
		if err == nil {
			return func() {
				_ = flockFile(int(f.Fd()), lockUnlock)
				_ = f.Close()
			}, nil
		}
		if !lockWouldBlock(err) {
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
