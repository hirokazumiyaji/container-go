//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

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
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "containergo-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
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
		case <-time.After(10 * time.Millisecond):
		}
	}
}
