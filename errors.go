package container

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As when the error is
// retained in the chain; the current liveness wrapper may flatten it
// into text instead (#104).
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that a backend CLI command returned a
// non-zero exit status and its follow-up liveness probe also failed. For
// Apple Container, start the system service with `container system start`;
// for Docker, start the Docker daemon. Missing or unlaunchable CLI
// binaries are launch errors and are not classified as ErrSystemNotRunning.
// The current classifier renders the original CLI error into the wrapper's
// text rather than preserving it as an unwrap target (#104).
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was neither declared with
// WithExposedPorts nor explicitly published, or for which the backend
// reported no usable host binding.
var ErrPortNotExposed = errors.New("port is not declared or has no usable host binding")

// ErrImageNotFound reports that the current backend precheck did not find
// an image in its local store. Run returns it for PullNever when that
// precheck reports the image absent; on Apple this is a best-effort
// backend-specific precheck, not a no-fetch guarantee (#112).
var ErrImageNotFound = errors.New("image not found in local store")

// ErrContainerNotFound reports a classified CLI failure that identifies a
// missing container. Inspect, State, Exec, and Logs wrap that failure with
// %w when the backend reports not-found text. A successful inspect response
// with no matching target is not guaranteed to produce this sentinel; empty
// or malformed inspect data can return a generic error, and the current
// Docker parser does not verify a returned object's ID or name (#103).
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that a delete-time generation check
// found a live container whose creation label no longer matches the
// handle. Current reuse does not perform a final generation check after
// readiness; issues #83 and #84 track that and the missing-generation
// cleanup paths.
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
// ErrContainerNotFound so errors.Is works from the root package. It does
// not normalize a successful empty or mismatched inspect result (#103).
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
