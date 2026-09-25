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

// ErrInvalidOption identifies an invalid public option or option value.
// Callers can use errors.Is without matching the human-readable message.
var ErrInvalidOption = errors.New("invalid option")

// ValidationError describes a public input rejected before a backend
// operation starts. Option and Field contain the public option name,
// Value contains the rejected value, and Message preserves the detailed
// explanation returned to the caller.
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
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
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

// OptionError is an option-specific name for ValidationError.
type OptionError = ValidationError

// InvalidOptionError is an alias for ValidationError.
type InvalidOptionError = ValidationError

func newValidationError(option string, value any, err error) error {
	if err == nil {
		err = ErrInvalidOption
	}
	var existing *ValidationError
	if errors.As(err, &existing) {
		return err
	}
	return &ValidationError{
		Option:  option,
		Field:   option,
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
