//go:build darwin || freebsd || linux || netbsd || openbsd

package container

import (
	"os"
	"syscall"
)

// openCopyFile refuses links and avoids a blocking open if the target
// changes to a FIFO after the Lstat check.
func openCopyFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
