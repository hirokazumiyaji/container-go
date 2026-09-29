//go:build aix || solaris

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Solaris and AIX do not expose flock(2) through the Go syscall package;
// their fcntl(F_SETLK) record locks provide the same cross-process name
// serialization for the generation-checked delete path.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "containergo-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
		err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
		if err == nil {
			return func() {
				unlockLock := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 0}
				_ = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &unlockLock)
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
		case <-time.After(10 * time.Millisecond):
		}
	}
}
