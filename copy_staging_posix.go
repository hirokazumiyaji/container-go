//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func prepareCopyStagingRoot() (string, error) {
	return prepareCopyStagingRootForSource(nil)
}

func prepareCopyStagingRootForSource(source *openedCopySource) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("copy to container: locate per-user cache: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("copy to container: resolve per-user cache: %w", err)
	}
	if err := ensureCopyStagingBase(base, source); err != nil {
		return "", fmt.Errorf("copy to container: ensure per-user cache: %w", err)
	}

	appRoot := filepath.Join(base, "containergo")
	if err := ensurePrivateCopyStagingDir(appRoot); err != nil {
		return "", fmt.Errorf("copy to container: create private staging parent: %w", err)
	}
	root := filepath.Join(appRoot, "copy-staging")
	if err := ensurePrivateCopyStagingDir(root); err != nil {
		return "", fmt.Errorf("copy to container: create private staging root: %w", err)
	}
	return root, nil
}

func ensureCopyStagingBase(path string, source *openedCopySource) error {
	if _, err := os.Lstat(path); err == nil {
		if err := verifyCopyStagingBase(path); err != nil {
			return err
		}
		return ensureCopyStagingOutsideSource(path, source)
	} else if !os.IsNotExist(err) {
		return err
	}

	parentPath := filepath.Dir(path)
	parent, reparse, err := openCopySource(parentPath)
	if err != nil {
		return fmt.Errorf("open parent of missing per-user cache %q: %w", path, err)
	}
	defer func() { _ = parent.Close() }()
	parentInfo, err := parent.Stat()
	if err != nil {
		return err
	}
	if reparse || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("parent of per-user cache %q is not a real directory", path)
	}
	if err := verifyCopyStagingAncestors(parent, parentInfo); err != nil {
		return err
	}
	if err := ensureOpenCopyDirectoryOutsideSource(parent, reparse, source); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent.Fd()), filepath.Base(path), 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := verifyCreatedCopyStagingBase(path); err != nil {
		return err
	}
	if err := verifyCopyStagingBase(path); err != nil {
		return err
	}
	return ensureCopyStagingOutsideSource(path, source)
}

func verifyCreatedCopyStagingBase(path string) error {
	file, reparse, err := openCopySource(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("created per-user cache %q permissions are %04o, want 0700", path, info.Mode().Perm())
	}
	return nil
}

func verifyCopyStagingBase(path string) error {
	file, reparse, err := openCopySource(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%q is not owned by the current user", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%q is group- or world-writable", path)
	}
	return verifyCopyStagingAncestors(file, info)
}

// verifyCopyStagingAncestors owns neither base nor the parent handles it
// opens while walking toward the filesystem root.
func verifyCopyStagingAncestors(base *os.File, baseInfo os.FileInfo) error {
	current := base
	currentInfo := baseInfo
	opened := make([]*os.File, 0, 8)
	defer func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}()
	for {
		parent, reparse, err := openCopySourceAt(current, "..")
		if err != nil {
			return fmt.Errorf("open parent of staging base: %w", err)
		}
		opened = append(opened, parent)
		parentInfo, err := parent.Stat()
		if err != nil {
			return err
		}
		if reparse || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("staging base has a non-directory ancestor")
		}
		parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
		if !ok || (int(parentStat.Uid) != 0 && int(parentStat.Uid) != os.Geteuid()) {
			return fmt.Errorf("staging base has an ancestor not owned by the current user or root")
		}
		if parentInfo.Mode().Perm()&0o022 != 0 && parentInfo.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("staging base has a group- or world-writable non-sticky ancestor")
		}
		if sameCopyStagingDirectory(currentInfo, parentInfo) {
			return nil
		}
		current = parent
		currentInfo = parentInfo
	}
}

func sameCopyStagingDirectory(first, second os.FileInfo) bool {
	firstStat, firstOK := first.Sys().(*syscall.Stat_t)
	secondStat, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && firstStat.Dev == secondStat.Dev && firstStat.Ino == secondStat.Ino
}

func ensurePrivateCopyStagingDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	file, reparse, err := openCopySource(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if reparse || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%q is not owned by the current user", path)
	}
	if err := file.Chmod(0o700); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if after.Mode().Perm() != 0o700 {
		return fmt.Errorf("%q permissions are %04o, want 0700", path, after.Mode().Perm())
	}
	return nil
}

func createCopyStagingDir(root string) (string, error) {
	dir, err := os.MkdirTemp(root, "copy-to-")
	if err != nil {
		return "", err
	}
	if err := ensurePrivateCopyStagingDir(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
