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

func chmodEnvDirectory(path string) error {
	f, err := openEnvFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return statErr
	}
	if !info.IsDir() {
		_ = f.Close()
		return fmt.Errorf("%w: %q is not a directory", errUnsafeEnvFile, path)
	}
	chmodErr := f.Chmod(envDirMode)
	closeErr := f.Close()
	return errors.Join(chmodErr, closeErr)
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

// canonicalEnvPath resolves existing symlink components once and returns
// the absolute path that callers must use for all later operations. The
// input is rejected if it contains '..'; a missing final component is
// allowed so ensureEnvFileBase can create it.
func canonicalEnvPath(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: cache path %q is not a valid absolute path", errUnsafeEnvFile, path)
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("%w: cache path %q contains '..'", errUnsafeEnvFile, path)
		}
	}
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("%w: resolve cache path %q: %v", errUnsafeEnvFile, parent, err)
	}
	if !filepath.IsAbs(resolvedParent) {
		return "", fmt.Errorf("%w: resolved cache path %q is not absolute", errUnsafeEnvFile, resolvedParent)
	}
	resolved := filepath.Clean(filepath.Join(resolvedParent, filepath.Base(clean)))
	if err := validateEnvPathComponents(filepath.Dir(resolved)); err != nil {
		return "", err
	}
	if info, err := os.Lstat(resolved); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: path %q is not a real directory", errUnsafeEnvFile, resolved)
		}
		if !trustedEnvPathComponent(info) {
			return "", fmt.Errorf("%w: path %q is writable by another user", errUnsafeEnvFile, resolved)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return resolved, nil
}

func validateEnvPathComponents(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return fmt.Errorf("%w: path %q is not absolute", errUnsafeEnvFile, path)
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("%w: path %q contains '..'", errUnsafeEnvFile, path)
		}
	}
	clean := filepath.Clean(path)
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: inspect path component %q: %v", errUnsafeEnvFile, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: path component %q is not a real directory", errUnsafeEnvFile, current)
		}
		if !trustedEnvPathComponent(info) {
			return fmt.Errorf("%w: path component %q is writable by another user", errUnsafeEnvFile, current)
		}
	}
	return nil
}
