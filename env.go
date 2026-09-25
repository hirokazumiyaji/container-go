package container

import (
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
	envFileDirPrefix = "containergo-env-"
	envFileLockName  = ".lock"
	envFileName      = "env"
	envFileMode      = 0o600
	envDirMode       = 0o700

	// A lock file makes a crashed writer distinguishable from a live
	// writer on Unix. The age fallback is for files made by older versions
	// (and for platforms without advisory file locks).
	envFileStaleAfter = 24 * time.Hour
)

// activeEnvFiles holds the lock descriptor for each env directory made by
// this process. Keeping the descriptor open keeps the advisory lock held
// until cleanup, even if the directory is unlinked.
var activeEnvFiles sync.Map // map[string]*os.File

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

// writeEnvFile stores env vars in a 0600 file under a 0700 temporary
// directory. The lock file is created before the env file is exposed to
// the backend, so a janitor can distinguish an active writer from a
// process that died without running its deferred cleanup.
func writeEnvFile(env map[string]string) (path, dir string, err error) {
	if err := validateEnvMap(env); err != nil {
		return "", "", err
	}

	cleanupStaleEnvFiles()
	dir, err = os.MkdirTemp("", envFileDirPrefix)
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(dir, envDirMode); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}

	lockPath := filepath.Join(dir, envFileLockName)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, envFileMode)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if err := lock.Chmod(envFileMode); err != nil {
		_ = lock.Close()
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if envFileLocksSupported {
		if err := acquireEnvFileLock(lock); err != nil {
			_ = lock.Close()
			_ = os.RemoveAll(dir)
			return "", "", err
		}
		activeEnvFiles.Store(dir, lock)
	} else {
		// Windows has no advisory flock. Keep the marker for the
		// age-based janitor, but do not leave a descriptor open that
		// would prevent a caller or cleanup from removing the directory.
		_ = lock.Close()
	}

	keep := false
	defer func() {
		if !keep {
			cleanupEnvFile(dir)
		}
	}()

	var b []byte
	for _, k := range sortedKeys(env) {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, env[k]...)
		b = append(b, '\n')
	}
	defer clear(b)

	path = filepath.Join(dir, envFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, envFileMode)
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

	keep = true
	return path, dir, nil
}

func closeEnvFileLock(lock *os.File) {
	if err := releaseEnvFileLock(lock); err != nil {
		log.Printf("container-go: release environment file lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		log.Printf("container-go: close environment file lock: %v", err)
	}
}

// cleanupEnvFile releases the writer lock and removes the complete
// private directory. It is idempotent so callers can use it both after
// the CLI returns and from a deferred failure path.
func cleanupEnvFile(dir string) {
	if dir == "" {
		return
	}
	if value, ok := activeEnvFiles.LoadAndDelete(dir); ok {
		closeEnvFileLock(value.(*os.File))
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("container-go: remove environment file: %v", err)
	}
}

// cleanupStaleEnvFiles reclaims directories left by a process that died
// before deferred cleanup ran. On Unix the writer holds an advisory lock
// for the whole backend call, so only an unlocked directory is removed.
// On platforms without that primitive, a conservative age threshold
// prevents an active long-running call from being mistaken for a crash.
func cleanupStaleEnvFiles() {
	tempDir := os.TempDir()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), envFileDirPrefix) {
			continue
		}
		dir := filepath.Join(tempDir, entry.Name())
		if _, active := activeEnvFiles.Load(dir); active {
			continue
		}
		modTime := envDirModTime(entry, now)
		// A writer creates and locks the marker before exposing the env
		// file. Ignore the short interval in which only the private
		// directory and lock exist, so a concurrent janitor cannot claim it.
		if _, err := os.Stat(filepath.Join(dir, envFileName)); err != nil {
			if os.IsNotExist(err) && now.Sub(modTime) >= envFileStaleAfter {
				removeStaleEnvDir(dir)
			}
			continue
		}
		if envFileLocksSupported {
			lock, err := os.OpenFile(filepath.Join(dir, envFileLockName), os.O_RDWR, envFileMode)
			if err != nil {
				// A directory without the new lock marker may belong to
				// an older library process. Reclaim it only when old.
				if os.IsNotExist(err) && now.Sub(modTime) >= envFileStaleAfter {
					removeStaleEnvDir(dir)
				}
				continue
			}
			locked, lockErr := tryAcquireEnvFileLock(lock)
			if lockErr != nil || !locked {
				_ = lock.Close()
				continue
			}
			removeStaleEnvDir(dir)
			closeEnvFileLock(lock)
			continue
		}
		if now.Sub(modTime) >= envFileStaleAfter {
			removeStaleEnvDir(dir)
		}
	}
}

func envDirModTime(entry os.DirEntry, fallback time.Time) time.Time {
	info, err := entry.Info()
	if err != nil {
		return fallback
	}
	return info.ModTime()
}

func removeStaleEnvDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("container-go: remove stale environment file: %v", err)
	}
}
