//go:build windows

package container

// Windows has no Unix setuid transition; the main-package build-mode guard
// in nativeReaperSupported is the remaining self-execution prerequisite.
func nativeReaperPlatformSupported() bool {
	return true
}
