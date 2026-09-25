package wait

import "errors"

var (
	// ErrInvalidConfiguration identifies a wait strategy that cannot be
	// executed with the configuration supplied by the caller. Validate and
	// container.Run return it before starting a container.
	ErrInvalidConfiguration = errors.New("invalid wait configuration")

	// ErrPortNotExposed identifies a wait target that does not declare the
	// requested port. The container package aliases this value for callers
	// that use container.ErrPortNotExposed.
	ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

	// ErrContainerNotFound identifies a wait target whose container no
	// longer exists. The container package aliases this value for callers
	// that use container.ErrContainerNotFound.
	ErrContainerNotFound = errors.New("container not found")
)
