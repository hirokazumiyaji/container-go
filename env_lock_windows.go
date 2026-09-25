//go:build windows

package container

import "os"

const envFileLocksSupported = false

func acquireEnvFileLock(*os.File) error {
	return nil
}

func tryAcquireEnvFileLock(*os.File) (bool, error) {
	return false, nil
}

func releaseEnvFileLock(*os.File) error {
	return nil
}
