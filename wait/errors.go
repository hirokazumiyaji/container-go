package wait

import (
	"errors"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ErrInvalidConfiguration classifies invalid static configuration passed
// to a wait strategy. Every such failure in this package is reported as a
// *ConfigError, so a caller can detect the whole class with one errors.Is.
var ErrInvalidConfiguration = errors.New("invalid wait strategy configuration")

// ErrPortNotExposed identifies a wait target that does not declare the
// requested port. The container package aliases this value for callers
// that use container.ErrPortNotExposed.
var ErrPortNotExposed = errors.New("port is not declared or has no usable host binding")

// ErrContainerNotFound identifies a wait target whose container no
// longer exists. The container package aliases this value for callers
// that use container.ErrContainerNotFound.
var ErrContainerNotFound = errors.New("container not found")

// ErrTargetNotFound can wrap a Target method error when the target no
// longer exists.
var ErrTargetNotFound = errors.New("wait target not found")

// ErrLogStreamSetup identifies deterministic log-stream setup failures
// that readiness strategies must not retry.
var ErrLogStreamSetup = cli.ErrStreamSetup

// ConfigError reports invalid static configuration passed to a wait strategy.
// It can be inspected with errors.As and classified with errors.Is using
// ErrInvalidConfiguration.
//
// It covers every static configuration error this package can report: a port
// specification that is not TCP or is out of range (ForListeningPort,
// ForHTTP), an empty exec command (ForExec), and an uncompilable log
// pattern (ForLog). Each fails immediately rather than being retried until
// the startup timeout.
type ConfigError struct {
	Strategy string
	Field    string
	Value    string
	Reason   string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("wait.%s: invalid %s %q: %s", e.Strategy, e.Field, e.Value, e.Reason)
}

func (e *ConfigError) Unwrap() error { return ErrInvalidConfiguration }
