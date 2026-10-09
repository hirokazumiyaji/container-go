//go:build windows

package container

import (
	"fmt"
	"os"
)

// The capability and flags are supplied by the Go-version-specific files
// because older Windows toolchains ignore the required CreateFile flags.
func checkCopyFileOpenCapability() error {
	if !windowsCopyFileOpenSupported {
		return fmt.Errorf(
			"%w: Windows Go 1.23 through 1.25 do not propagate no-follow and overlapped open flags; use Go 1.26+",
			ErrCopyFileFromContainerUnsupported,
		)
	}
	return nil
}

// openCopyFile opens the reparse point itself and uses overlapped I/O
// so a named-pipe target cannot turn the read into an unbounded wait.
func openCopyFile(name string) (*os.File, error) {
	if err := checkCopyFileOpenCapability(); err != nil {
		return nil, err
	}
	return os.OpenFile(name, os.O_RDONLY|windowsCopyFileOpenFlags, 0)
}
