//go:build !aix && !solaris && (darwin || dragonfly || freebsd || linux || netbsd || openbsd)

//nolint:unused // retained for platform-specific process-identity tests
package container

import "syscall"

func reaperProcessGroupID(pid int) (int, error) {
	return syscall.Getpgid(pid)
}
