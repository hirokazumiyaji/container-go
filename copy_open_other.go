//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

import "os"

func openCopySource(path string) (*os.File, bool, error) {
	return nil, false, unsupportedCopySource(path)
}

func openCopySourceAt(_ *os.File, path string) (*os.File, bool, error) {
	return nil, false, unsupportedCopySource(path)
}

func isCopySourceLinkError(error) bool { return false }
