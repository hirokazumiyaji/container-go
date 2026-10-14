//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

import (
	"fmt"
	"os"
)

func checkCopyFileOpenCapability() error {
	return fmt.Errorf("%w: host lacks no-follow/nonblocking copy-out open", ErrCopyFileFromContainerUnsupported)
}

// openCopyFile is retained for builds on unsupported hosts; callers
// reject the operation before invoking the backend CLI.
func openCopyFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY, 0)
}

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

func isCopyStagingRootParentError(error) bool { return false }
