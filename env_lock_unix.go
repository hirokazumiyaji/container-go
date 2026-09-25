//go:build !windows

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const envFileLocksSupported = true

func ensureEnvFileSecurity() error {
	return nil
}

func acquireEnvFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func acquireEnvFileRootLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func tryAcquireEnvFileLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func releaseEnvFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func openEnvFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func currentEnvFileUID() uint32 {
	return uint32(os.Getuid())
}

func envFileUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return stat.Uid, true
}

func validateEnvPathComponents(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: path %q is not absolute", errUnsafeEnvFile, path)
	}
	clean := filepath.Clean(path)
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("%w: inspect path component %q: %v", errUnsafeEnvFile, current, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: path component %q is not a directory", errUnsafeEnvFile, current)
		}
	}
	return nil
}
