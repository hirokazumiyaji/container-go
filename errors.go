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

// ErrGenerationReplaced reports that the live container is no longer the
// generation an operation inspected. Terminate and WithReuse refuse to
// act on it. The wording is retained for compatibility with callers that
// exposed the original delete-specific error text.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

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
