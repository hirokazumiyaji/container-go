//go:build solaris

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func lockName(ctx context.Context, name string) (func(), error) {
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
