//go:build !windows

package container

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func writeEnvMarker(path, contents string) (err error) {
	f, err := openEnvFileNoFollow(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(envFileMode); err != nil {
		return err
	}
	if err := writeAll(f, contents); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		// Leave the descriptor owned by the defer so a close error cannot
		// skip the final close attempt and leave the marker path behind.
		return err
	}
	f = nil
	return validatePrivateRegularFile(path)
}

func writeAll(w io.Writer, contents string) error {
	n, err := io.WriteString(w, contents)
	if err != nil {
		return err
	}
	if n != len(contents) {
		return io.ErrShortWrite
	}
	return nil
}

func validateEnvCleanupPath(root, dir string) error {
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("%w: cleanup path %q contains '..'", errUnsafeEnvFile, dir)
		}
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) != root {
		return fmt.Errorf("%w: cleanup path %q is outside root %q", errUnsafeEnvFile, dir, root)
	}
	name := filepath.Base(clean)
	if name == "." || name == ".." ||
		(!strings.HasPrefix(name, envFileDirPrefix) &&
			!strings.HasPrefix(name, envFileStagingPrefix) &&
			!strings.HasPrefix(name, envFileTombstonePrefix)) {
		return fmt.Errorf("%w: cleanup path %q is not a library directory", errUnsafeEnvFile, dir)
	}
	return nil
}

func validatedExpectedEnvChildren(dir string) (map[string]os.FileInfo, error) {
	return validatedExpectedEnvChildrenAt(dir, nil)
}

func validatedExpectedEnvChildrenAt(dir string, expectedDirInfo os.FileInfo) (map[string]os.FileInfo, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if expectedDirInfo != nil && !os.SameFile(expectedDirInfo, info) {
		return nil, fmt.Errorf("%w: environment directory %q was replaced before child validation", errUnsafeEnvFile, dir)
	}
	if err := validatePrivateDirectoryInfo(dir, info); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		envFileName:          true,
		envFileLockName:      true,
		envFileDirMarkerName: true,
	}
	validated := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return nil, fmt.Errorf("%w: environment directory %q contains unexpected child %q", errUnsafeEnvFile, dir, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		childInfo, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if err := validatePrivateRegularInfo(path, childInfo); err != nil {
			return nil, err
		}
		validated[entry.Name()] = childInfo
	}
	return validated, nil
}

func validateExpectedEnvChildren(dir string) error {
	_, err := validatedExpectedEnvChildren(dir)
	return err
}

func removeExpectedEnvChildrenWithChildren(dir string, expectedDirInfo os.FileInfo, expectedChildren map[string]os.FileInfo, allowMissing bool) error {
	// Validate the complete allowlist first. Never use RemoveAll here:
	// a foreign child added after validation must make cleanup fail
	// closed rather than be recursively deleted.
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if expectedDirInfo != nil && !os.SameFile(expectedDirInfo, dirInfo) {
		return fmt.Errorf("%w: environment directory %q was replaced before removal", errUnsafeEnvFile, dir)
	}
	for name, expected := range expectedChildren {
		if err := validateEnvChildIdentity(filepath.Join(dir, name), expected, allowMissing); err != nil {
			return err
		}
	}
	validated, err := validatedExpectedEnvChildrenAt(dir, dirInfo)
	if err != nil {
		return err
	}
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		path := filepath.Join(dir, name)
		if hook := envFileBeforeChildUnlinkHook; hook != nil {
			hook(path)
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		expected, ok := validated[name]
		if !ok {
			if bound, boundOK := expectedChildren[name]; boundOK {
				expected, ok = bound, true
			}
		}
		if !ok {
			return fmt.Errorf("%w: environment child %q appeared after validation", errUnsafeEnvFile, path)
		}
		if err := validatePrivateRegularInfo(path, info); err != nil {
			return err
		}
		if !os.SameFile(expected, info) {
			return fmt.Errorf("%w: environment child %q was replaced during removal", errUnsafeEnvFile, path)
		}
		if err := os.Remove(path); err != nil {
			if expectedChildren != nil && errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: environment child %q disappeared before unlink", errUnsafeEnvFile, path)
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	current, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: environment directory %q disappeared during removal", errUnsafeEnvFile, dir)
		}
		return err
	}
	if !os.SameFile(dirInfo, current) {
		return fmt.Errorf("%w: environment directory %q was replaced during removal", errUnsafeEnvFile, dir)
	}
	if err := os.Remove(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: environment directory %q disappeared during removal", errUnsafeEnvFile, dir)
		}
		return err
	}
	return nil
}

func validateEnvDirIdentity(path string, expected os.FileInfo) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: environment directory %q is unavailable: %v", errUnsafeEnvFile, path, err)
		}
		return err
	}
	if expected == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !os.SameFile(expected, info) {
		return fmt.Errorf("%w: environment directory %q was replaced", errUnsafeEnvFile, path)
	}
	return nil
}

func validatePrivateEnvDirIdentity(path string, expected os.FileInfo) error {
	if err := validateEnvDirIdentity(path, expected); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if expected == nil || !os.SameFile(expected, info) {
		return fmt.Errorf("%w: environment directory %q was replaced", errUnsafeEnvFile, path)
	}
	return validatePrivateDirectoryInfo(path, info)
}

func recordEnvChildInfo(state *activeEnvFile, name, path string) error {
	// Hold an open descriptor to the child inode. On Linux, a bare
	// FileInfo snapshot is not enough: after unlink the inode number can
	// be reused by a replacement created at the same path, and SameFile
	// would then treat the replacement as the original.
	f, err := openEnvFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		_ = f.Close()
		return err
	}
	confirmed, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return err
	}
	if !os.SameFile(info, confirmed) {
		_ = f.Close()
		return fmt.Errorf("%w: environment child %q was replaced during creation", errUnsafeEnvFile, path)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.childInfos == nil {
		state.childInfos = make(map[string]os.FileInfo)
	}
	if state.childFiles == nil {
		state.childFiles = make(map[string]*os.File)
	}
	if previous, ok := state.childInfos[name]; ok && !os.SameFile(previous, info) {
		_ = f.Close()
		return fmt.Errorf("%w: environment child %q was replaced during creation", errUnsafeEnvFile, path)
	}
	if old := state.childFiles[name]; old != nil {
		_ = old.Close()
	}
	state.childInfos[name] = info
	state.childFiles[name] = f
	return nil
}

func validateEnvChildIdentity(path string, expected os.FileInfo, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if allowMissing {
			return nil
		}
		return fmt.Errorf("%w: environment child %q is missing", errUnsafeEnvFile, path)
	}
	if err != nil {
		return err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		return err
	}
	if expected == nil || !os.SameFile(expected, info) {
		return fmt.Errorf("%w: environment child %q was replaced", errUnsafeEnvFile, path)
	}
	return nil
}

func validateEnvChildIdentities(state *activeEnvFile, dir string, allowMissing bool) error {
	if state == nil {
		return nil
	}
	for name, info := range state.childInfos {
		if err := validateEnvChildIdentity(filepath.Join(dir, name), info, allowMissing); err != nil {
			return err
		}
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validatePrivateDirectoryInfo(path, info)
}

func validatePrivateDirectoryInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", errUnsafeEnvFile, path)
	}
	if err := validateEnvOwnership(info, currentEnvFileUID(), envDirMode); err != nil {
		return fmt.Errorf("%w: %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func validatePrivateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validatePrivateRegularInfo(path, info)
}

func validatePrivateRegularInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file", errUnsafeEnvFile, path)
	}
	if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
		return fmt.Errorf("%w: %q: %v", errUnsafeEnvFile, path, err)
	}
	return nil
}

func validateEnvOwnership(info os.FileInfo, wantOwner uint32, wantMode os.FileMode) error {
	owner, ok := envFileUID(info)
	if !ok || owner != wantOwner {
		return fmt.Errorf("owner does not match the current user")
	}
	if info.Mode().Perm() != wantMode.Perm() {
		return fmt.Errorf("mode is %04o, want %04o", info.Mode().Perm(), wantMode.Perm())
	}
	return nil
}

func validateEnvMarkerIfPresent(dir string) error {
	marker, _, err := openValidatedEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return marker.Close()
}

func repairEnvMarker(path, contents string) (err error) {
	f, err := openEnvFileNoFollow(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: environment marker %q is unsafe: %v", errUnsafeEnvFile, path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		return fmt.Errorf("%w: environment marker %q is unsafe: %v", errUnsafeEnvFile, path, err)
	}
	if info.Size() > int64(len(contents)) {
		return fmt.Errorf("%w: environment marker %q is too long", errUnsafeEnvFile, path)
	}
	data := make([]byte, info.Size())
	if len(data) > 0 {
		n, readErr := f.ReadAt(data, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != len(data) || !strings.HasPrefix(contents, string(data)) {
			return fmt.Errorf("%w: environment marker %q has unsafe contents", errUnsafeEnvFile, path)
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return fmt.Errorf("%w: environment marker %q was replaced", errUnsafeEnvFile, path)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return writeAll(f, contents)
}

func openValidatedEnvMarker(path, contents string, flag int) (*os.File, os.FileInfo, error) {
	f, err := openEnvFileNoFollow(path, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if info.Size() != int64(len(contents)) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q has invalid length", errUnsafeEnvFile, path)
	}
	buf := make([]byte, len(contents))
	n, readErr := f.ReadAt(buf, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		_ = f.Close()
		return nil, nil, readErr
	}
	if n != len(contents) || string(buf) != contents {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q has invalid contents", errUnsafeEnvFile, path)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: marker %q was replaced", errUnsafeEnvFile, path)
	}
	return f, info, nil
}

func openValidatedPrivateFile(path string) (*os.File, os.FileInfo, error) {
	f, err := openEnvFileNoFollow(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if err := validatePrivateRegularInfo(path, info); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%w: file %q was replaced", errUnsafeEnvFile, path)
	}
	return f, info, nil
}
