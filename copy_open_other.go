//go:build !darwin && !freebsd && !linux && !netbsd && !openbsd && !windows

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
