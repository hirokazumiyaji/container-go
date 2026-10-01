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

// ErrInvalidConfig reports an option combination that the selected
// backend cannot honor. Run returns it as a *ConfigError.
var ErrInvalidConfig = errors.New("invalid container configuration")

// ConfigError describes an invalid option combination before container
// creation. Callers can use errors.As to inspect the backend, network,
// and option involved.
type ConfigError struct {
	Backend string
	Network string
	Option  string
	Detail  string
}

func (e *ConfigError) Error() string {
	scope := "container"
	if e.Backend != "" {
		scope = e.Backend
	}
	message := ErrInvalidConfig.Error() + ": " + scope + " configuration"
	if e.Network != "" {
		message += fmt.Sprintf(" for network %q", e.Network)
	}
	if e.Option != "" {
		message += " (" + e.Option + ")"
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

func (e *ConfigError) Unwrap() error { return ErrInvalidConfig }

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts or WithPublishedPort, or that has no usable host
// binding in the backend's actual network mode.
var ErrPortNotExposed = errors.New("port is not declared or has no usable host binding")

// ErrEndpointUnreachable reports an inspected host binding that cannot
// be reached by this client, such as loopback on a remote Docker daemon.
var ErrEndpointUnreachable = errors.New("container endpoint is unreachable")

// ErrNetworkMismatch reports that the network reported by inspect does
// not match the network requested for the handle.
var ErrNetworkMismatch = errors.New("container network mode does not match the requested network")

// ErrNoReachableHost reports a backend network that has no host endpoint.
var ErrNoReachableHost = errors.New("container has no reachable host")

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that a handle's immutable identity or
// generation no longer matches the live container. Destructive and endpoint
// operations refuse to act on the replacement.
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
