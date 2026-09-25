package container

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
type CLIError = cli.CLIError

// CleanupError reports a primary operation failure together with a failure
// from automatic cleanup or verification of the retained container. Both
// causes remain available through errors.Is/errors.As.
type CleanupError struct {
	Err        error
	CleanupErr error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup failed: %v", e.Err, e.CleanupErr)
}

// Unwrap preserves both the operation and cleanup branches.
func (e *CleanupError) Unwrap() []error {
	return []error{e.Err, e.CleanupErr}
}

func withCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return &CleanupError{Err: err, CleanupErr: cleanupErr}
}

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts.
var ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that an operation refused to use or
// delete a live container because its creation generation or backend
// identity no longer matches the handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// ErrNameLockCompatibility reports that a name-addressed operation could
// not establish every historical lock namespace required during migration.
var ErrNameLockCompatibility = errors.New("name lock compatibility")

// isNotFound reports whether a CLI failure means the container does not
// exist. Matching substrings live on each engine (see engine_*.go).
func isNotFound(err error) bool {
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	return appleEngine{}.containerMissing(err) || dockerEngine{}.containerMissing(err)
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
