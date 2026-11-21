//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"fmt"
	"os"
	"path/filepath"
)

func prepareCopyStagingRoot() (string, func() error, error) {
	return prepareCopyStagingRootForSource(nil)
}

func prepareCopyStagingRootForSource(source *openedCopySource) (string, func() error, error) {
	appRoot, err := ensureContainergoCacheRoot(func(dir *os.File, reparse bool) error {
		return ensureOpenCopyDirectoryOutsideSource(dir, reparse, source)
	})
	if err != nil {
		return "", nil, fmt.Errorf("copy staging: %w", err)
	}
	root := filepath.Join(appRoot, "copy-staging")
	if err := ensurePrivateCacheDir(root, nil); err != nil {
		return "", nil, fmt.Errorf("copy staging: create private staging root: %w", err)
	}
	return root, noCopyStagingRootCleanup, nil
}

func createCopyStagingDir(root string) (string, error) {
	dir, err := os.MkdirTemp(root, "copy-to-")
	if err != nil {
		return "", err
	}
	if err := ensurePrivateCacheDir(dir, nil); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
