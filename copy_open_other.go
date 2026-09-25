//go:build !darwin && !freebsd && !linux && !netbsd && !openbsd && !windows

package container

import "os"

// openCopyFile relies on the preceding Lstat on platforms without a
// native no-follow open flag.
func openCopyFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY, 0)
}
