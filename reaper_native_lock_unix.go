//go:build !windows

package container

import (
	"errors"
	"os"
	"time"
)

// lockReaperName takes the same per-name flock the Go name lock uses. The
// lock file is validated as private before it is opened, so a substituted
// symlink, FIFO, or foreign file is skipped instead of followed. Acquisition
// is bounded: a wedged lock holder must not stall every entry behind it.
func lockReaperName(path string, timeout time.Duration) (func(), error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errReaperLockSkipped
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	f, err := openVerifiedLockFile(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
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
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, errReaperLockTimeout
		}
		time.Sleep(nameLockPoll)
	}
}
