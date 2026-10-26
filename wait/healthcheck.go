package wait

import (
	"context"
	"fmt"
	"time"
)

// HealthcheckStrategy waits until the container's Docker HEALTHCHECK
// reports healthy.
type HealthcheckStrategy struct {
	options
}

// ForHealthcheck waits for the image's Docker HEALTHCHECK to report
// healthy. Docker reports starting while the check is still running, which
// keeps polling; unhealthy is terminal. An image without a HEALTHCHECK fails
// immediately with a ConfigError, and a target that cannot report healthcheck
// status (including Apple Container) is rejected rather than retried.
func ForHealthcheck() *HealthcheckStrategy {
	// Health reads the Docker adapter's lifecycle snapshot, which the poll's
	// 1 Hz state probe refreshes. Polling faster than that cadence does not
	// spawn additional CLI processes.
	return &HealthcheckStrategy{options: options{stateFirst: true}}
}

func (s *HealthcheckStrategy) WithStartupTimeout(d time.Duration) *HealthcheckStrategy {
	s.startupTimeout = d
	return s
}

func (s *HealthcheckStrategy) WithPollInterval(d time.Duration) *HealthcheckStrategy {
	s.pollInterval = d
	return s
}

func (s *HealthcheckStrategy) validate() error {
	return s.options.validate()
}

func (s *HealthcheckStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeDiagnosticError(retErr) }()
	if err := s.validate(); err != nil {
		return err
	}
	healthTarget, ok := target.(HealthTarget)
	if !ok {
		return &ConfigError{
			Strategy: "ForHealthcheck",
			Field:    "target",
			Value:    fmt.Sprintf("%T", target),
			Reason:   "target does not report Docker HEALTHCHECK status",
		}
	}

	return poll(ctx, s.options, target, "wait for container healthcheck", func(ctx context.Context) error {
		health, err := healthTarget.Health(ctx)
		if err != nil {
			return err
		}
		switch health {
		case HealthHealthy:
			return nil
		case HealthStarting:
			return fmt.Errorf("health status %q", health)
		case HealthNone:
			// Docker reports a null State.Health only when the image
			// defines no HEALTHCHECK; no retry can add one.
			return fatalCheckError{err: &ConfigError{
				Strategy: "ForHealthcheck",
				Field:    "healthcheck",
				Reason:   "image does not define a HEALTHCHECK",
			}}
		case HealthUnhealthy:
			return fatalCheckError{err: fmt.Errorf("container healthcheck reported unhealthy")}
		default:
			return fatalCheckError{err: &ConfigError{
				Strategy: "ForHealthcheck",
				Field:    "health status",
				Value:    string(health),
				Reason:   "unknown Docker HEALTHCHECK status",
			}}
		}
	})
}

// RequiresHealthcheck reports whether strategy waits on Docker HEALTHCHECK
// status, including through composite strategies. Backends without
// healthchecks use it to reject a strategy before creating a container.
func RequiresHealthcheck(strategy Strategy) bool {
	switch s := strategy.(type) {
	case *HealthcheckStrategy:
		return true
	case *AllStrategy:
		for _, child := range s.strategies {
			if RequiresHealthcheck(child) {
				return true
			}
		}
	case *AnyStrategy:
		// Any succeeds through one supported branch, so it needs
		// healthcheck support only when every branch does.
		if len(s.strategies) == 0 {
			return false
		}
		for _, child := range s.strategies {
			if !RequiresHealthcheck(child) {
				return false
			}
		}
		return true
	}
	return false
}
