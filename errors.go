package container

import (
	"context"
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package. The public four-field layout is retained for
// compatibility; classification may use additional bounded diagnostics.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the selected container backend is
// not running. The classified error retains the backend-specific hint.
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

// isNotFound reports whether a CLI failure means the container does not
// exist. It is retained for callers that do not have an engine context;
// the concrete backend and command still have to pass their own matcher.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	if cliErr.Binary != "" {
		if cliBinaryMatches(cliErr.Binary, "docker") {
			return (dockerEngine{}).containerMissing(err)
		}
		return (appleEngine{}).containerMissing(err)
	}
	// Empty Binary is the historical default `container` executable.
	return (appleEngine{}).containerMissing(err)
}

// isNotFoundFor applies the selected backend's classifier. Keeping this
// separate from isNotFound prevents a Docker error from being accepted by
// an Apple operation (and vice versa) when callers do have engine context.
func isNotFoundFor(eng engine, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	if eng == nil {
		return isNotFound(err)
	}
	return eng.containerMissing(err)
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	return wrapNotFoundFor(nil, err)
}

// wrapNotFoundFor applies the selected backend before converting a
// classified CLI not-found failure into ErrContainerNotFound.
func wrapNotFoundFor(eng engine, err error) error {
	if err == nil || !isNotFoundFor(eng, err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

// classifyError applies the backend liveness contract, while refusing to
// probe an ambiguous application message such as a generic "container not
// found" from a run/exec process. The original CLIError is then returned
// unchanged even if a runner would report its probe as unavailable.
func classifyError(ctx context.Context, r cli.Runner, err error, eng engine) error {
	if err == nil {
		return nil
	}
	if eng == nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return errors.Join(err, ctxErr)
		}
		return wrapNotFound(err)
	}
	if isAmbiguousApplicationError(eng, err) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return errors.Join(err, ctxErr)
		}
		return err
	}
	return cli.Classify(ctx, r, err, eng.probe())
}
