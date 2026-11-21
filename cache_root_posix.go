//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// containergoCacheRootCheck inspects a verified directory handle before the
// shared cache root is built on it. Copy staging passes a check that keeps the
// staging root outside the opened copy source; other callers pass nil.
type containergoCacheRootCheck func(dir *os.File, reparse bool) error

func runContainergoCacheRootCheck(check containergoCacheRootCheck, dir *os.File, reparse bool) error {
	if check == nil {
		return nil
	}
	return check(dir, reparse)
}

// ensureContainergoCacheRoot returns the private <UserCacheDir>/containergo
// directory, creating it when missing. Both the cache base and the returned
// directory are validated with the checks the copy staging path established:
// no symlink is followed, both are owned by the current user and are not
// group- or world-writable, every ancestor is owned by the current user or
// root, and a writable ancestor is accepted only when it is sticky. A missing
// base is created with mode 0700 through its verified parent handle, so the
// helper never creates a directory chain under a path it has not inspected.
func ensureContainergoCacheRoot(check containergoCacheRootCheck) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate per-user cache: %w", err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve per-user cache: %w", err)
	}
	if err := ensureContainergoCacheBase(base, check); err != nil {
		return "", fmt.Errorf("ensure per-user cache: %w", err)
	}
	appRoot := filepath.Join(base, "containergo")
	if err := ensurePrivateCacheDir(appRoot, check); err != nil {
		return "", fmt.Errorf("create private cache directory %q: %w", appRoot, err)
	}
	return appRoot, nil
}

// ensureContainergoCacheBase verifies an existing cache base or creates a
// missing one privately. Callers that must keep the cache outside another
// directory supply check, which runs on the verified parent before creation.
func ensureContainergoCacheBase(path string, check containergoCacheRootCheck) error {
	if _, err := os.Lstat(path); err == nil {
		return verifyContainergoCacheBase(path, check)
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
	if err := verifyContainergoCacheAncestors(parent, parentInfo); err != nil {
		return err
	}
	if err := runContainergoCacheRootCheck(check, parent, reparse); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent.Fd()), filepath.Base(path), 0o700); err == nil {
		if err := verifyCreatedContainergoCacheBase(path); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	return verifyContainergoCacheBase(path, check)
}

// verifyCreatedContainergoCacheBase checks the mode of a base this process
// created. A concurrent creator makes Mkdirat report EEXIST, and the base it
// left behind is verified by verifyContainergoCacheBase instead, which admits
// the usual ~/.cache mode of 0755.
func verifyCreatedContainergoCacheBase(path string) error {
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

func verifyContainergoCacheBase(path string, check containergoCacheRootCheck) error {
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
	if err := verifyContainergoCacheAncestors(file, info); err != nil {
		return err
	}
	return runContainergoCacheRootCheck(check, file, reparse)
}

// verifyContainergoCacheAncestors owns neither base nor the parent handles it
// opens while walking toward the filesystem root. It validates base itself too,
// so a missing cache base is rejected before its creation when the parent it
// would be created in fails the ancestor rules.
func verifyContainergoCacheAncestors(base *os.File, baseInfo os.FileInfo) error {
	if err := verifyContainergoCacheAncestor(baseInfo); err != nil {
		return err
	}
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
			return fmt.Errorf("open parent of per-user cache: %w", err)
		}
		opened = append(opened, parent)
		parentInfo, err := parent.Stat()
		if err != nil {
			return err
		}
		if reparse || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("per-user cache has a non-directory ancestor")
		}
		if err := verifyContainergoCacheAncestor(parentInfo); err != nil {
			return err
		}
		if sameContainergoCacheDirectory(currentInfo, parentInfo) {
			return nil
		}
		current = parent
		currentInfo = parentInfo
	}
}

func verifyContainergoCacheAncestor(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != 0 && int(stat.Uid) != os.Geteuid()) {
		return fmt.Errorf("per-user cache has an ancestor not owned by the current user or root")
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("per-user cache has a group- or world-writable non-sticky ancestor")
	}
	return nil
}

func sameContainergoCacheDirectory(first, second os.FileInfo) bool {
	firstStat, firstOK := first.Sys().(*syscall.Stat_t)
	secondStat, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && firstStat.Dev == secondStat.Dev && firstStat.Ino == secondStat.Ino
}

// ensurePrivateCacheDir creates path if needed with mode 0700, refuses a
// substituted or foreign directory, and tightens the mode of an existing
// directory. check, when non-nil, inspects the opened directory before it is
// accepted.
func ensurePrivateCacheDir(path string, check containergoCacheRootCheck) error {
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
	return runContainergoCacheRootCheck(check, file, reparse)
}
