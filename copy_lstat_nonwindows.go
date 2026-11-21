//go:build !windows

package container

import "os"

// lstatCopyFile records the device and inode at call time, so os.SameFile
// compares the opened handle with the file that was actually inspected.
func lstatCopyFile(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}
