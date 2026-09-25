//go:build windows

package container

import (
	"os"
	"syscall"
)

// openCopySource asks Windows not to follow a final reparse point. The
// Lstat/identity check in copy.go still rejects links and other changes.
func openCopySource(path string) (*os.File, error) {
	handle, err := syscall.Open(path, syscall.O_RDONLY|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
