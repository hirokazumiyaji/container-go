//go:build !windows

package container

import "os"

// nativeReaperPlatformSupported rejects a setuid or setgid host: the
// self-executed child would run with different effective credentials, and a
// caller-controlled environment or argv would become an execution entry
// point with those credentials. There is no /bin/sh fallback; such a host has
// no reaper and registration is a no-op.
func nativeReaperPlatformSupported() bool {
	return os.Getuid() == os.Geteuid() && os.Getgid() == os.Getegid()
}
