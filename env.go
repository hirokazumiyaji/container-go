package container

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	envFileRootName       = "containergo-env-v1"
	envFileRootMarkerName = ".root"
	envFileRootLockMarker = "container-go secure environment-file root v1\n"

	envFileDirPrefix     = "containergo-env-"
	envFileStagingPrefix = ".containergo-env-new-"
	envFileDirMarkerName = ".container-go-env"
	envFileDirMarker     = "container-go environment directory v1\n"
	envFileLockName      = ".lock"
	envFileName          = "env"
	envFileMode          = 0o600
	envDirMode           = 0o700

	// A process that dies while staging releases the root lock, so an
	// abandoned staging directory is eligible for an age-based retry. Fully
	// initialized directories are protected only by their writer lock; age is
	// deliberately not a liveness signal.
	envFileStagingStaleAfter = 24 * time.Hour
)

var (
	errUnsafeEnvFile = errors.New("environment file storage failed safety validation")

	// activeEnvFiles holds the lock descriptor and identity of each env
	// directory made by this process. The descriptor remains open for the
	// whole backend call, even if the directory is unlinked.
	activeEnvFiles sync.Map // map[string]activeEnvFile

	// pendingEnvCleanups records a trusted directory whose first removal
	// failed. A later deferred retry may continue even if RemoveAll already
	// removed the marker before encountering the original error.
	pendingEnvCleanups sync.Map // map[string]activeEnvFile
)

type activeEnvFile struct {
	lock    *os.File
	dirInfo os.FileInfo
	root    string
}

// validateEnvMap enforces the format understood by both supported
// backends before any CLI command is built. Env-file lines are UTF-8 and
// line-delimited: keys cannot be empty, comments, whitespace, control
// characters, or contain the '=' delimiter. Values may contain spaces and
// '=' but must be valid UTF-8 and free of control characters or line
// separators (including NUL and line breaks).
func validateEnvMap(env map[string]string) error {
	key, value, err := firstInvalidEnv(env)
	if err == nil {
		return nil
	}
	if value {
		return fmt.Errorf("environment variable %s: value %w", key, err)
	}
	return fmt.Errorf("invalid environment variable name %q: %w", key, err)
}

func firstInvalidEnv(env map[string]string) (key string, value bool, err error) {
	for k, v := range env {
		if err := validateEnvKey(k); err != nil {
			return k, false, err
		}
		if err := validateEnvValue(v); err != nil {
			return k, true, err
		}
	}
	return "", false, nil
}

func validateEnvKey(k string) error {
	if k == "" {
		return fmt.Errorf("must not be empty")
	}
	if !utf8.ValidString(k) {
		return fmt.Errorf("must be valid UTF-8")
	}
	if strings.HasPrefix(k, "#") {
		return fmt.Errorf("must not start with '#'")
	}
	// Docker strips a UTF-8 BOM from the first env-file line. Reject it
	// here rather than allowing a key to be silently renamed or treated
	// as a comment.
	if strings.HasPrefix(k, "\uFEFF") {
		return fmt.Errorf("must not start with a byte-order mark")
	}
	for _, r := range k {
		switch {
		case r == '=':
			return fmt.Errorf("must not contain '='")
		case r == 0:
			return fmt.Errorf("must not contain NUL")
		case r == '\n' || r == '\r':
			return fmt.Errorf("must not contain line breaks")
		case unicode.IsSpace(r):
			return fmt.Errorf("must not contain whitespace")
		case unicode.IsControl(r):
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

func validateEnvValue(v string) error {
	if !utf8.ValidString(v) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range v {
		switch {
		case r == 0:
			return fmt.Errorf("must not contain NUL")
		case r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029':
			return fmt.Errorf("must not contain line breaks")
		case unicode.IsControl(r):
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

// writeEnvFile stores env vars in a 0600 file under a 0700 per-user cache
// directory. It never uses TMPDIR. On Windows it fails with
// ErrEnvFileUnsupported because Go's chmod modes do not provide the claimed
// per-user secrecy there.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	return writeEnvFileAt("", env)
}

func writeEnvFileAt(base string, env map[string]string) (path, dir string, err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return "", "", err
	}
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}
	err = withEnvFileRoot(base, func(root string) error {
		if err := cleanupEnvDirs(root); err != nil {
			return err
		}
		var err error
		path, dir, err = createEnvFile(root, env)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return path, dir, nil
}

func createEnvFile(root string, env map[string]string) (path, dir string, err error) {
	staging, err := os.MkdirTemp(root, envFileStagingPrefix)
	if err != nil {
		return "", "", err
	}
	keep := false
	cleanupDir := staging
	var lock *os.File
	defer func() {
		if keep {
			return
		}
		var cleanupErr error
		if lock != nil {
			cleanupErr = closeEnvFileLock(lock)
		}
		if removeErr := os.RemoveAll(cleanupDir); removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
		err = errors.Join(err, cleanupErr)
	}()

	if err := os.Chmod(staging, envDirMode); err != nil {
		return "", "", err
	}
	if err := validatePrivateDirectory(staging); err != nil {
		return "", "", err
	}
	if err := writeEnvMarker(filepath.Join(staging, envFileDirMarkerName), envFileDirMarker); err != nil {
		return "", "", err
	}

	lockPath := filepath.Join(staging, envFileLockName)
	lock, err = openEnvFileNoFollow(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		return "", "", err
	}
	if err := lock.Chmod(envFileMode); err != nil {
		return "", "", err
	}
	if err := validatePrivateRegularFile(lockPath); err != nil {
		return "", "", err
	}
	if err := acquireEnvFileLock(lock); err != nil {
		return "", "", err
	}

	var b []byte
	for _, k := range sortedKeys(env) {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, env[k]...)
		b = append(b, '\n')
	}
	defer clear(b)

	envPath := filepath.Join(staging, envFileName)
	f, err := openEnvFileNoFollow(envPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, envFileMode)
	if err != nil {
		return "", "", err
	}
	if err := f.Chmod(envFileMode); err != nil {
		_ = f.Close()
		return "", "", err
	}
	n, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil {
		return "", "", writeErr
	}
	if n != len(b) {
		return "", "", io.ErrShortWrite
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if err := validatePrivateRegularFile(envPath); err != nil {
		return "", "", err
	}

	suffix := strings.TrimPrefix(filepath.Base(staging), envFileStagingPrefix)
	if suffix == "" {
		return "", "", fmt.Errorf("%w: invalid staging directory name %q", errUnsafeEnvFile, staging)
	}
	dir = filepath.Join(root, envFileDirPrefix+suffix)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("%w: environment directory %q already exists", errUnsafeEnvFile, dir)
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", "", err
	}
	cleanupDir = dir
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	activeEnvFiles.Store(dir, activeEnvFile{lock: lock, dirInfo: dirInfo, root: root})
	keep = true
	path = filepath.Join(dir, envFileName)
	return path, dir, nil
}

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
		f = nil
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

func closeEnvFileLock(lock *os.File) error {
	var errs []error
	if err := releaseEnvFileLock(lock); err != nil {
		errs = append(errs, err)
	}
	if err := lock.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cleanupEnvFile releases the writer lock and removes the complete private
// directory. It is idempotent. Removal and lock-close failures are returned;
// callers also retain the directory for a deferred retry.
func cleanupEnvFile(dir string) error {
	return cleanupEnvFileAt("", dir, os.RemoveAll)
}

func cleanupEnvFileAt(base, dir string, removeAll func(string) error) error {
	if dir == "" {
		return nil
	}
	if removeAll == nil {
		return errors.New("environment cleanup: nil remove function")
	}

	value, active := activeEnvFiles.Load(dir)
	entry, hasEntry := value.(activeEnvFile)
	if !hasEntry && active {
		return fmt.Errorf("environment cleanup: invalid active entry for %q", dir)
	}
	pendingValue, isPending := pendingEnvCleanups.Load(dir)
	if isPending {
		entry = pendingValue.(activeEnvFile)
		hasEntry = true
		active = false
	}

	var lock *os.File
	if hasEntry {
		lock = entry.lock
	}
	removeStarted := false
	cleanupErr := withEnvFileRoot(base, func(root string) error {
		if filepath.Clean(filepath.Dir(dir)) != root {
			return fmt.Errorf("%w: cleanup path %q is outside root %q", errUnsafeEnvFile, dir, root)
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			pendingEnvCleanups.Delete(dir)
			activeEnvFiles.Delete(dir)
			return nil
		}
		if err != nil {
			return err
		}
		if err := validatePrivateDirectoryInfo(dir, info); err != nil {
			return err
		}
		if hasEntry {
			if entry.root != root || !os.SameFile(entry.dirInfo, info) {
				return fmt.Errorf("%w: environment directory %q was replaced", errUnsafeEnvFile, dir)
			}
			if lock != nil {
				lockInfo, err := lock.Stat()
				if err != nil {
					return err
				}
				lockPath := filepath.Join(dir, envFileLockName)
				if err := validatePrivateRegularInfo(lockPath, lockInfo); err != nil {
					return err
				}
				pathInfo, err := os.Lstat(lockPath)
				if err != nil {
					return err
				}
				if !os.SameFile(lockInfo, pathInfo) {
					return fmt.Errorf("%w: environment lock %q was replaced", errUnsafeEnvFile, lockPath)
				}
			}
		}

		cleanupLock := lock
		if cleanupLock == nil {
			var err error
			cleanupLock, _, err = openValidatedPrivateFile(filepath.Join(dir, envFileLockName))
			if err != nil {
				if isPending && errors.Is(err, os.ErrNotExist) {
					cleanupLock = nil
				} else {
					return err
				}
			}
		}
		if cleanupLock != nil && cleanupLock != lock {
			locked, err := tryAcquireEnvFileLock(cleanupLock)
			if err != nil {
				_ = cleanupLock.Close()
				return err
			}
			if !locked {
				_ = cleanupLock.Close()
				return fmt.Errorf("environment cleanup: directory %q is active", dir)
			}
			defer func() { _ = cleanupLock.Close() }()
		}
		if !isPending {
			marker, _, err := openValidatedEnvMarker(filepath.Join(dir, envFileDirMarkerName), envFileDirMarker, os.O_RDONLY)
			if err != nil {
				return err
			}
			if err := marker.Close(); err != nil {
				return err
			}
		}

		removeStarted = true
		return removeAll(dir)
	})

	if active && hasEntry {
		activeEnvFiles.Delete(dir)
	}
	if removeStarted {
		if cleanupErr == nil {
			pendingEnvCleanups.Delete(dir)
		} else {
			info, infoErr := os.Lstat(dir)
			if infoErr == nil {
				pendingEnvCleanups.Store(dir, activeEnvFile{dirInfo: info, root: filepath.Dir(dir)})
			}
		}
	}
	if lock != nil {
		cleanupErr = errors.Join(cleanupErr, closeEnvFileLock(lock))
	}
	return cleanupErr
}

func retryEnvFileCleanup(dir *string) {
	if dir == nil || *dir == "" {
		return
	}
	if err := cleanupEnvFile(*dir); err != nil {
		log.Printf("container-go: retry environment file cleanup: %v", err)
	}
}

func joinEnvFileCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, cleanupErr)
}

// cleanupStaleEnvFiles reclaims directories left by a process that died
// before deferred cleanup ran. It is intentionally not available on Windows.
// Unix writers hold a non-blocking advisory lock for the whole backend call;
// only an unlocked, marked, owned 0700 directory with the exact expected
// children can be removed.
func cleanupStaleEnvFiles() error {
	return cleanupStaleEnvFilesAt("")
}

func cleanupStaleEnvFilesAt(base string) error {
	if err := ensureEnvFileSecurity(); err != nil {
		return err
	}
	return withEnvFileRoot(base, cleanupEnvDirs)
}

func withEnvFileRoot(base string, fn func(string) error) (err error) {
	if err := ensureEnvFileSecurity(); err != nil {
		return err
	}
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locate per-user environment-file root: %w", err)
		}
	}
	root, err := ensureEnvFileRoot(base)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(root, envFileRootMarkerName)
	marker, _, err := openValidatedEnvMarker(markerPath, envFileRootLockMarker, os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeEnvMarker(markerPath, envFileRootLockMarker); err != nil {
			return err
		}
		marker, _, err = openValidatedEnvMarker(markerPath, envFileRootLockMarker, os.O_RDWR)
	}
	if err != nil {
		return err
	}
	if err := acquireEnvFileRootLock(marker); err != nil {
		_ = marker.Close()
		return err
	}
	defer func() {
		err = errors.Join(err, releaseEnvFileRootLock(marker))
		if closeErr := marker.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	markerInfo, err := marker.Stat()
	if err != nil {
		return err
	}
	if err := validatePrivateRegularInfo(markerPath, markerInfo); err != nil {
		return err
	}
	pathInfo, err := os.Lstat(markerPath)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(markerInfo, pathInfo) {
		return fmt.Errorf("%w: environment root marker %q was replaced", errUnsafeEnvFile, markerPath)
	}
	if err := validatePrivateDirectory(root); err != nil {
		return err
	}
	return fn(root)
}

func releaseEnvFileRootLock(f *os.File) error {
	return releaseEnvFileLock(f)
}

func ensureEnvFileRoot(base string) (string, error) {
	if err := ensureEnvFileBase(base); err != nil {
		return "", err
	}
	root := filepath.Join(base, envFileRootName)
	err := os.Mkdir(root, envDirMode)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if created {
		if err := os.Chmod(root, envDirMode); err != nil {
			return "", err
		}
	}
	if err := validateEnvPathComponents(root); err != nil {
		return "", err
	}
	if err := validatePrivateDirectory(root); err != nil {
		return "", err
	}
	return root, nil
}

func ensureEnvFileBase(base string) error {
	if !filepath.IsAbs(base) {
		return fmt.Errorf("%w: cache path %q is not absolute", errUnsafeEnvFile, base)
	}
	parent := filepath.Dir(filepath.Clean(base))
	if err := validateEnvPathComponents(parent); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !trustedEnvParent(parentInfo) {
		return fmt.Errorf("%w: cache parent %q is not private to this user", errUnsafeEnvFile, parent)
	}

	err = os.Mkdir(base, envDirMode)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if created {
		if err := os.Chmod(base, envDirMode); err != nil {
			return err
		}
	}
	if err := validateEnvPathComponents(base); err != nil {
		return err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return err
	}
	uid := currentEnvFileUID()
	owner, ok := envFileUID(info)
	if !ok || owner != uid {
		return fmt.Errorf("%w: cache directory %q is not owned by this user", errUnsafeEnvFile, base)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: cache directory %q is group- or world-writable", errUnsafeEnvFile, base)
	}
	return nil
}

func trustedEnvParent(info os.FileInfo) bool {
	uid := currentEnvFileUID()
	owner, ok := envFileUID(info)
	if !ok {
		return false
	}
	mode := info.Mode()
	if owner == uid {
		return mode.Perm()&0o022 == 0
	}
	// A sticky world-writable directory such as /tmp still prevents other
	// users from replacing a child they do not own.
	return mode.Perm()&0o002 != 0 && mode&os.ModeSticky != 0
}

func cleanupEnvDirs(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, envFileDirPrefix) && !strings.HasPrefix(name, envFileStagingPrefix) {
			continue
		}
		dir := filepath.Join(root, name)
		if _, active := activeEnvFiles.Load(dir); active {
			continue
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: stale environment entry %q is not a directory", errUnsafeEnvFile, dir)
		}
		if strings.HasPrefix(name, envFileDirPrefix) {
			if err := removeStaleEnvDir(dir); err != nil {
				return err
			}
			continue
		}
		if now.Sub(info.ModTime()) < envFileStagingStaleAfter {
			continue
		}
		if err := removeStaleStagingEnvDir(dir, info); err != nil {
			return err
		}
	}
	return nil
}

func removeStaleEnvDir(dir string) (err error) {
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	marker, _, err := openValidatedEnvMarker(markerPath, envFileDirMarker, os.O_RDONLY)
	if err != nil {
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}

	lockPath := filepath.Join(dir, envFileLockName)
	lock, lockInfo, err := openValidatedPrivateFile(lockPath)
	if err != nil {
		return err
	}
	locked, err := tryAcquireEnvFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !locked {
		return lock.Close()
	}
	defer func() {
		err = errors.Join(err, closeEnvFileLock(lock))
	}()

	currentDirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	currentLockInfo, err := os.Lstat(lockPath)
	if err != nil {
		return err
	}
	if !os.SameFile(dirInfo, currentDirInfo) || !os.SameFile(lockInfo, currentLockInfo) {
		return fmt.Errorf("%w: stale environment directory %q was replaced during validation", errUnsafeEnvFile, dir)
	}
	if err := validateExpectedEnvChildren(dir); err != nil {
		return err
	}
	return removeExpectedEnvChildren(dir)
}

func removeStaleStagingEnvDir(dir string, dirInfo os.FileInfo) error {
	if err := validatePrivateDirectoryInfo(dir, dirInfo); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(dir, envFileDirMarkerName)
	_, markerErr := os.Lstat(markerPath)
	if errors.Is(markerErr, os.ErrNotExist) {
		if len(entries) != 0 {
			return fmt.Errorf("%w: unmarked staging directory %q contains unexpected children", errUnsafeEnvFile, dir)
		}
		current, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !os.SameFile(dirInfo, current) {
			return fmt.Errorf("%w: staging directory %q was replaced", errUnsafeEnvFile, dir)
		}
		return os.Remove(dir)
	}
	if markerErr != nil {
		return markerErr
	}
	return removeStaleEnvDir(dir)
}

func validateExpectedEnvChildren(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		envFileName:          true,
		envFileLockName:      true,
		envFileDirMarkerName: true,
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("%w: environment directory %q contains unexpected child %q", errUnsafeEnvFile, dir, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: environment child %q is not a regular file", errUnsafeEnvFile, path)
		}
		if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
			return err
		}
	}
	return nil
}

func removeExpectedEnvChildren(dir string) error {
	for _, name := range []string{envFileName, envFileDirMarkerName, envFileLockName} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateEnvOwnership(info, currentEnvFileUID(), envFileMode); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return os.Remove(dir)
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
