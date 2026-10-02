//go:build !windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

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
// Apple Container does not offer. The lock file lives in the temp
// directory and is never removed, since removing it would race with a
// concurrent locker.
func lockName(ctx context.Context, name string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "containergo-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
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
