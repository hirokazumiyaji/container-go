//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

import "os"

func openCopySource(path string) (*os.File, error) {
	return os.Open(path)
}
