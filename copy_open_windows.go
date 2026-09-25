//go:build windows

package container

import (
	"os"
	"syscall"
)

// openCopyFile opens the reparse point itself and uses overlapped I/O
// so a named-pipe target cannot turn the read into an unbounded wait.
func openCopyFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_OVERLAPPED, 0)
}
