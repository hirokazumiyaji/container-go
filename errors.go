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

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// ErrExecTerminationUnsupported reports that Exec stopped its local CLI
// process after a context deadline or cancellation, but could not prove
// that the backend-side exec process stopped. Neither supported CLI
// exposes a common exec-instance kill operation.
var ErrExecTerminationUnsupported = errors.New("exec process termination is unsupported by backend")

// ExecTerminationError preserves the context or infrastructure error while
// making the possible daemon-side process leak explicit. Callers should
// terminate the container or use a backend-specific cleanup mechanism when
// this error is returned.
type ExecTerminationError struct {
	// Err is the context or infrastructure error that stopped local Exec.
	Err error
}

func (e *ExecTerminationError) Error() string {
	if e == nil || e.Err == nil {
		return ErrExecTerminationUnsupported.Error()
	}
	return fmt.Sprintf("%s: %v (terminate the container or use backend-specific cleanup)", ErrExecTerminationUnsupported, e.Err)
}

func (e *ExecTerminationError) Unwrap() error { return e.Err }

func (e *ExecTerminationError) Is(target error) bool {
	return target == ErrExecTerminationUnsupported
}

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
