package container

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. Its Error method
// redacts diagnostic secrets and escapes control characters; RawError is
// the explicit local-debugging escape hatch. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
//
// The alias keeps the exported Binary/Args/ExitCode/Stderr fields and keyed
// composite literals source-compatible. The safe wrapper adds unexported
// state to retain the original value for RawError; consequently external
// packages must use keyed CLIError literals rather than unkeyed literals.
// This is the deliberate compatibility tradeoff for safe errors.As results.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// SystemNotRunningError is the typed classification returned when both the
// original CLI operation and its liveness probe fail. Its unwrap chain keeps
// ErrSystemNotRunning, the original error, and the probe error; use
// OriginalError or ProbeError when both errors have the same concrete type.
type SystemNotRunningError = cli.SystemNotRunningError

// ErrInvalidOption identifies a rejected functional option. Option values
// are intentionally not stored in the validation error: rejected values may
// be credentials or other sensitive material.
var ErrInvalidOption = errors.New("invalid option")

// OptionError is the public-boundary fallback for a functional option that
// returns an untyped error. Its original cause remains in the unwrap chain,
// but its rendering is intentionally value-free.
type OptionError struct {
	cause error
}

func (e *OptionError) Error() string { return "invalid option" }
func (e *OptionError) Unwrap() error { return e.cause }
func (e *OptionError) Is(target error) bool {
	return target == ErrInvalidOption || errors.Is(e.cause, target)
}

// ValidationError describes a rejected option without echoing its value.
// Callers can use errors.Is(err, ErrInvalidOption) or errors.As for this
// typed diagnostic.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ErrInvalidOption.Error()
	}
	if e.Problem == "" {
		return "invalid " + e.Field
	}
	return "invalid " + e.Field + ": " + e.Problem
}

func (e *ValidationError) Unwrap() error { return ErrInvalidOption }

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
