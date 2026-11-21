//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"os"
	"syscall"
)

func checkCopyFileOpenCapability() error { return nil }

// openCopyFile refuses links and avoids a blocking open if the target
// changes to a FIFO after the Lstat check.
func openCopyFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
