//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

func prepareCopyStagingRoot() (string, error) {
	return prepareCopyStagingRootForSource(nil)
}

func prepareCopyStagingRootForSource(*openedCopySource) (string, error) {
	return "", unsupportedCopySource("copy staging")
}

func createCopyStagingDir(string) (string, error) {
	return "", unsupportedCopySource("copy staging")
}
