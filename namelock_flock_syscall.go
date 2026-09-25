//go:build !windows && !solaris && !illumos

package container

import (
	"errors"
	"syscall"
)

const (
	lockExclusiveNonBlocking = syscall.LOCK_EX | syscall.LOCK_NB
	lockUnlock               = syscall.LOCK_UN
)

func flockFile(fd, how int) error { return syscall.Flock(fd, how) }

func lockWouldBlock(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }
