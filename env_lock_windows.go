//go:build windows

package container

import "os"

const envFileLocksSupported = false

func ensureEnvFileSecurity() error {
	return ErrEnvFileUnsupported
}

func acquireEnvFileLock(*os.File) error {
	return ErrEnvFileUnsupported
}

func acquireEnvFileRootLock(*os.File) error {
	return ErrEnvFileUnsupported
}

func tryAcquireEnvFileLock(*os.File) (bool, error) {
	return false, ErrEnvFileUnsupported
}

func releaseEnvFileLock(*os.File) error {
	return ErrEnvFileUnsupported
}

func chmodEnvDirectory(string) error {
	return ErrEnvFileUnsupported
}

func openEnvFileNoFollow(string, int, os.FileMode) (*os.File, error) {
	return nil, ErrEnvFileUnsupported
}

func currentEnvFileUID() uint32 {
	return 0
}

func envFileUID(os.FileInfo) (uint32, bool) {
	return 0, false
}

func canonicalEnvPath(string) (string, error) {
	return "", ErrEnvFileUnsupported
}

func validateEnvPathComponents(string) error {
	return ErrEnvFileUnsupported
}
