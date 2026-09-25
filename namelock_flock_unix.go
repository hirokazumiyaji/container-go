//go:build solaris || illumos

package container

import (
	"errors"

	"golang.org/x/sys/unix"
)

const (
	lockExclusiveNonBlocking = unix.LOCK_EX | unix.LOCK_NB
	lockUnlock               = unix.LOCK_UN
)

func flockFile(fd, how int) error { return unix.Flock(fd, how) }

func lockWouldBlock(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) }
