//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package container

func prepareCopyStagingRoot() (string, error) {
	return "", unsupportedCopySource("copy staging")
}

func createCopyStagingDir(string) (string, error) {
	return "", unsupportedCopySource("copy staging")
}
