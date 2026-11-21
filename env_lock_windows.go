//go:build windows

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const envFileLocksSupported = true

// ensureEnvFileSecurity reports whether this platform can store secrets in a
// per-user protected location. Windows can: env_windows.go creates the root,
// each environment directory, and the env file itself with a protected DACL
// that only grants the current logon (plus Administrators and LOCAL SYSTEM)
// access, and rejects reparse points at every level.
func ensureEnvFileSecurity() error {
	return nil
}

// canonicalWindowsEnvBase resolves the per-user cache base once and returns
// the absolute, non-reparse path later operations must use. An empty base
// selects %LocalAppData%, matching os.UserCacheDir.
func canonicalWindowsEnvBase(base string) (string, error) {
	if base == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("%w: locate per-user cache directory: %v", errUnsafeEnvFile, err)
		}
		base = dir
	}
	if base == "" || strings.IndexByte(base, 0) >= 0 || !filepath.IsAbs(base) || filepath.VolumeName(base) == "" {
		return "", fmt.Errorf("%w: cache path %q is not a valid absolute path", errUnsafeEnvFile, base)
	}
	for _, part := range strings.FieldsFunc(base, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == ".." {
			return "", fmt.Errorf("%w: cache path %q contains '..'", errUnsafeEnvFile, base)
		}
	}
	clean := filepath.Clean(base)
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("%w: inspect cache path %q: %v", errUnsafeEnvFile, clean, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: cache path %q is not a real directory", errUnsafeEnvFile, clean)
	}
	if err := verifyWindowsEnvPathComponents(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// ensureWindowsEnvRoot creates (or revalidates) the per-user environment root
// with a protected DACL and returns its canonical path.
func ensureWindowsEnvRoot(base string) (string, error) {
	resolved, err := canonicalWindowsEnvBase(base)
	if err != nil {
		return "", err
	}
	root := filepath.Join(resolved, envFileRootName)
	if err := ensureWindowsPrivateDir(root, true); err != nil {
		return "", fmt.Errorf("%w: environment root %q: %v", errUnsafeEnvFile, root, err)
	}
	if err := verifyWindowsEnvPathComponents(filepath.Dir(root)); err != nil {
		return "", err
	}
	f, err := openWindowsEnvDirHandle(root)
	if err != nil {
		return "", fmt.Errorf("%w: environment root %q: %v", errUnsafeEnvFile, root, err)
	}
	defer func() { _ = f.Close() }()
	if err := verifyWindowsPrivateDirHandle(f, root); err != nil {
		return "", err
	}
	return root, nil
}

// verifyWindowsEnvPathComponents rejects reparse points and non-directories
// in every component of an existing path so a junction planted by another
// user cannot redirect the environment root.
func verifyWindowsEnvPathComponents(path string) error {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return fmt.Errorf("%w: path %q has no volume", errUnsafeEnvFile, path)
	}
	rest := strings.TrimLeft(strings.TrimPrefix(filepath.Clean(path), volume), `\/`)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := verifyWindowsEnvPathComponent(current); err != nil {
			return err
		}
	}
	return nil
}

func verifyWindowsEnvPathComponent(path string) error {
	f, err := openWindowsEnvDirHandle(path)
	if err != nil {
		return fmt.Errorf("%w: inspect path component %q: %v", errUnsafeEnvFile, path, err)
	}
	defer func() { _ = f.Close() }()
	reparse, err := windowsCopyHandleIsReparse(f)
	if err != nil {
		return err
	}
	if reparse {
		return fmt.Errorf("%w: path component %q is a reparse point", errUnsafeEnvFile, path)
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: path component %q is not a real directory", errUnsafeEnvFile, path)
	}
	return nil
}

// openWindowsEnvDirHandle opens a directory without following a reparse
// point. The descriptor is later validated with verifyWindowsPrivateDirHandle
// before it is trusted.
func openWindowsEnvDirHandle(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func verifyWindowsPrivateDirHandle(f *os.File, path string) error {
	reparse, err := windowsCopyHandleIsReparse(f)
	if err != nil {
		return err
	}
	if reparse {
		return fmt.Errorf("%w: directory %q is a reparse point", errUnsafeEnvFile, path)
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a real directory", errUnsafeEnvFile, path)
	}
	if err := verifyWindowsPrivateOwner(f); err != nil {
		return fmt.Errorf("%w: directory %q: %v", errUnsafeEnvFile, path, err)
	}
	if err := verifyCurrentUserOnlyDACL(f); err != nil {
		return fmt.Errorf("%w: directory %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func verifyWindowsPrivateDir(path string) error {
	f, err := openWindowsEnvDirHandle(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return verifyWindowsPrivateDirHandle(f, path)
}

// createWindowsPrivateFileHandle creates a new file whose initial security
// descriptor restricts access to the current logon. CREATE_NEW is deliberate:
// an existing path is either a stale artifact the cleanup path owns or a
// replacement attempt, and neither may be opened for writing here.
func createWindowsPrivateFileHandle(path string, access uint32) (*os.File, error) {
	attributes, err := currentUserSecurityAttributes()
	if err != nil {
		return nil, err
	}
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		copySourceShareMode,
		attributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		handle, err = windows.CreateFile(
			pathPtr,
			access|windows.READ_CONTROL|windows.WRITE_DAC,
			copySourceShareMode,
			attributes,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
	}
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openOrCreateWindowsPrivateFileHandle(path string) (*os.File, error) {
	attributes, err := currentUserSecurityAttributes()
	if err != nil {
		return nil, err
	}
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		copySourceShareMode,
		attributes,
		windows.OPEN_ALWAYS,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openWindowsPrivateFileHandle(path string, access uint32) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access|windows.READ_CONTROL,
		copySourceShareMode,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// hardenWindowsPrivateFile applies and verifies the current-logon-only DACL
// on a handle opened with WRITE_DAC access.
//
// New objects should belong to this process, but GitHub-hosted Windows
// runners have been observed to assign new objects to BUILTIN\Administrators.
// Repair the owner when the handle allows it and accept the same privileged
// SIDs the DACL allowlist already trusts, matching copy staging.
func hardenWindowsPrivateFile(f *os.File) error {
	_ = setCurrentUserOwner(f)
	if err := verifyWindowsPrivateOwner(f); err != nil {
		return err
	}
	if err := setCurrentUserOnlyDACL(f); err != nil {
		return err
	}
	if err := verifyCurrentUserOnlyDACL(f); err != nil {
		// A first SetSecurityInfo can convert inherited ACEs to explicit
		// ones without dropping them. Retry once against the now-protected
		// descriptor.
		if retryErr := setCurrentUserOnlyDACL(f); retryErr != nil {
			return err
		}
		if err := verifyCurrentUserOnlyDACL(f); err != nil {
			return err
		}
	}
	return verifyWindowsPrivateOwner(f)
}

// verifyWindowsPrivateOwner accepts the owner SIDs the private DACL already
// trusts. A foreign user's SID is rejected; Administrators and LOCAL SYSTEM
// are granted access by the DACL itself, so an object they own is not a
// weaker guarantee than the copy-staging policy.
func verifyWindowsPrivateOwner(f *os.File) error {
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(f.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil {
		return fmt.Errorf("object has no owner SID")
	}
	allowed, err := allowedPrivateSIDs()
	if err != nil {
		return err
	}
	if !sidAllowedForPrivate(owner, allowed) {
		return fmt.Errorf("object is owned by a foreign SID")
	}
	return nil
}

func verifyWindowsPrivateFileHandle(f *os.File, path string) error {
	reparse, err := windowsCopyHandleIsReparse(f)
	if err != nil {
		return err
	}
	if reparse {
		return fmt.Errorf("%w: file %q is a reparse point", errUnsafeEnvFile, path)
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file", errUnsafeEnvFile, path)
	}
	if err := verifyWindowsPrivateOwner(f); err != nil {
		return fmt.Errorf("%w: file %q: %v", errUnsafeEnvFile, path, err)
	}
	if err := verifyCurrentUserOnlyDACL(f); err != nil {
		return fmt.Errorf("%w: file %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func verifyWindowsPrivateFile(path string) error {
	f, err := openWindowsPrivateFileHandle(path, windows.FILE_GENERIC_READ)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return verifyWindowsPrivateFileHandle(f, path)
}

func readWindowsPrivateFile(path string) ([]byte, error) {
	f, err := openWindowsPrivateFileHandle(path, windows.FILE_GENERIC_READ)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := verifyWindowsPrivateFileHandle(f, path); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func createWindowsPrivateFile(path string, data []byte) error {
	f, err := createWindowsPrivateFileHandle(path, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		return err
	}
	if err := hardenWindowsPrivateFile(f); err != nil {
		_ = f.Close()
		return err
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return verifyWindowsPrivateFile(path)
}

// openWindowsEnvRootLock opens the root's advisory lock file, creating it
// with a protected DACL on first use.
func openWindowsEnvRootLock(root string) (*os.File, error) {
	path := filepath.Join(root, envFileLockName)
	f, err := openOrCreateWindowsPrivateFileHandle(path)
	if err != nil {
		return nil, fmt.Errorf("%w: environment root lock %q: %v", errUnsafeEnvFile, path, err)
	}
	if err := verifyWindowsPrivateFileHandle(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// acquireWindowsRootLockContext polls a non-blocking LockFileEx so a canceled
// Run or cleanup operation releases the descriptor and returns its context
// error promptly instead of parking in a blocking lock.
func acquireWindowsRootLockContext(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryAcquireWindowsFileLock(f)
		if err != nil {
			return err
		}
		if locked {
			if err := ctx.Err(); err != nil {
				_ = releaseWindowsFileLock(f)
				return err
			}
			return nil
		}
		timer := time.NewTimer(envRootLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func tryAcquireWindowsFileLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func releaseWindowsFileLock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
