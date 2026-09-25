//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"os"
	"syscall"
)

// openCopySource prevents a final-component symlink from being followed
// and keeps a raced FIFO open from blocking the snapshot.
func openCopySource(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
