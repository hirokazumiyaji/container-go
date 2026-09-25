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

// ErrInvalidOption identifies an invalid public option, operation argument,
// or option value. Callers can use errors.Is without matching the
// human-readable message, or use errors.As with *ValidationError for field
// metadata.
var ErrInvalidOption = errors.New("invalid option")

// ValidationError describes a public input rejected before a backend
// operation starts.
//
// Option names the public option or operation. Field names the exact input
// field, such as "key", "value", "hostPath", or "containerPath". Value is
// the rejected value when it is safe to expose; values that may contain
// credentials or other sensitive material are represented by nil. Message is
// retained for source compatibility, but Err is the source of truth for the
// rendered message and the error chain.
type ValidationError struct {
	Option  string
	Field   string
	Value   any
	Message string
	Err     error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	// Derive the message from Err so callers cannot make Message and Err
	// disagree by mutating the exported compatibility field.
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Message != "" {
		return e.Message
	}
	return ErrInvalidOption.Error()
}

// Unwrap preserves an underlying parse or validation error when one is
// available. Is classifies every ValidationError as ErrInvalidOption.
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ValidationError) Is(target error) bool {
	if e == nil {
		return false
	}
	if target == ErrInvalidOption {
		return true
	}
	return e.Err != nil && errors.Is(e.Err, target)
}

func newValidationError(option string, value any, err error) error {
	return newValidationErrorWithField(option, option, value, err)
}

func newValidationErrorWithField(option, field string, value any, err error) error {
	if err == nil {
		err = ErrInvalidOption
	}
	var existing *ValidationError
	if errors.As(err, &existing) {
		return err
	}
	return &ValidationError{
		Option:  option,
		Field:   field,
		Value:   value,
		Message: err.Error(),
		Err:     err,
	}
}

func validationErrorf(option string, value any, format string, args ...any) error {
	return newValidationError(option, value, fmt.Errorf(format, args...))
}

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
