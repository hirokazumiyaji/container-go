package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The per-user environment storage is implemented per platform: env_unix.go
// relies on POSIX ownership, modes, and advisory locks, while env_windows.go
// uses Windows DACLs and LockFileEx. This file holds the parts both share:
// constant names, validation policy, and budget/error helpers.

const (
	envFileRootName       = "containergo-env-v1"
	envFileRootMarkerName = ".root"
	envFileRootLockMarker = "container-go secure environment-file root v1\n"

	envFileDirPrefix       = "containergo-env-"
	envFileStagingPrefix   = ".containergo-env-new-"
	envFileTombstonePrefix = ".containergo-env-gone-"
	envFileDirMarkerName   = ".container-go-env"
	envFileDirMarker       = "container-go environment directory v1\n"
	envFileLockName        = ".lock"
	envFileName            = "env"
	envFileMode            = 0o600
	envDirMode             = 0o700

	// A process that dies while staging releases the root lock, so an
	// abandoned staging directory is eligible for an age-based retry. Fully
	// initialized directories are protected only by their writer lock; age is
	// deliberately not a liveness signal.
	envFileStagingStaleAfter = 24 * time.Hour

	// envDirGCInterval bounds how often the write path scans the cache root
	// for abandoned directories while holding the root lock. A staging
	// directory is independent of that scan, so reclaiming on every
	// WithEnv/WithExecEnv call only lengthened the critical section. Scans
	// triggered by Run's explicit security preflight stay unconditional.
	envDirGCInterval = 30 * time.Second
)

// envDirGC tracks the last successful reclaim scan per cache root so the
// write path does not repeat a readdir/Lstat sweep under the root lock. It is
// shared by both platforms: Windows performs the same expensive DACL
// validation on every entry, so it uses the same throttle.
var (
	envDirGCMu   sync.Mutex
	envDirGCLast = map[string]time.Time{}
)

var (
	// Root-lock polling and cleanup retries are deliberately bounded. The
	// context supplied by Run/Exec still takes precedence when it expires
	// sooner.
	envRootLockPollInterval     = 10 * time.Millisecond
	envFileSecurityTimeout      = 30 * time.Second
	envCleanupBackgroundTimeout = 30 * time.Second
)

var errUnsafeEnvFile = errors.New("environment file storage failed safety validation")

func boundedEnvContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, envFileSecurityTimeout)
}

func joinEnvFileCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, cleanupErr)
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

// envDirGCDue reports whether root has no recorded scan inside the throttle
// window.
func envDirGCDue(root string, now time.Time) bool {
	envDirGCMu.Lock()
	defer envDirGCMu.Unlock()
	last, ok := envDirGCLast[root]
	return !ok || now.Sub(last) >= envDirGCInterval
}

// markEnvDirGC records a successful reclaim scan for root.
func markEnvDirGC(root string, now time.Time) {
	envDirGCMu.Lock()
	envDirGCLast[root] = now
	envDirGCMu.Unlock()
}
