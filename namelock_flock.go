//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package container

import (
	"os"
	"syscall"
)

func tryLockNameFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockNameFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
