//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

import "os"

func openCopySource(path string) (*os.File, bool, error) {
	return nil, false, unsupportedCopySource(path)
}

func openCopySourceAt(_ *os.File, path string) (*os.File, bool, error) {
	return nil, false, unsupportedCopySource(path)
}

func copySourceFileIdentity(*os.File) (copySourceIdentity, error) {
	return copySourceIdentity{}, unsupportedCopySource("copy source identity")
}

func sameCopySourceIdentity(copySourceIdentity, copySourceIdentity) bool { return false }

func isCopySourceLinkError(error) bool { return false }

func isCopySourceUnsupportedOpenError(error) bool { return false }
