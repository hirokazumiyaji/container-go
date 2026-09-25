package wait

import (
	"errors"
	"fmt"
)

// ErrInvalidConfiguration classifies invalid static configuration passed
// to a wait strategy.
var ErrInvalidConfiguration = errors.New("invalid wait strategy configuration")

// ConfigError reports invalid static configuration passed to a wait strategy.
// It can be inspected with errors.As and classified with errors.Is using
// ErrInvalidConfiguration.
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
