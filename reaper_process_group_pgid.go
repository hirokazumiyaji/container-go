//go:build !aix && !solaris && (darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package container

import "syscall"

func reaperProcessGroupID(pid int) (int, error) {
	return syscall.Getpgid(pid)
}
