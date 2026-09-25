//go:build !windows && !solaris

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func reaperNameLockPath(name string) string {
	return filepath.Join(os.TempDir(), "containergo-"+name+".lock")
}

func ensureReaperNameLock(name string) error {
	f, err := os.OpenFile(reaperNameLockPath(name), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// lockName serializes generation-checked, name-addressed deletes of one
// container name across processes on this host. Apple Container has no
// immutable container ID, so an inspect-then-delete by name is only
// safe if no other process can delete and recreate the name in between;
// every such delete in this library takes this lock first. That covers
// cooperating processes using this library only: a direct `container`
// CLI invocation or another implementation does not take the lock and
// can still replace the name inside the window. Closing that would
// need an immutable ID or an atomic conditional delete from the
// backend, which Apple Container does not offer. The lock file lives
// in the temp directory and is never removed, since removing it would
// race with a concurrent locker.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	path := reaperNameLockPath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	releaseGate, err := acquireNameLockGate(ctx, path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if runtime.GOOS == "linux" {
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return func() {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
					releaseGate()
				}, nil
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				_ = f.Close()
				releaseGate()
				return nil, err
			}
			select {
			case <-ctx.Done():
				_ = f.Close()
				releaseGate()
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	for {
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
		err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
		if err == nil {
			return func() {
				unlock := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 0}
				_ = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &unlock)
				_ = f.Close()
				releaseGate()
			}, nil
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EACCES) {
			_ = f.Close()
			releaseGate()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			releaseGate()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
