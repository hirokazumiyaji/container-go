// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.
package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultPollInterval   = 100 * time.Millisecond
	// stateCheckInterval bounds how often the fail-fast state probe
	// spawns a CLI process during polling.
	stateCheckInterval = time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port means the first declared
	// port.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// Strategy waits until a started container is ready for use.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
}

func (o options) effective() (timeout, interval time.Duration) {
	timeout, interval = o.startupTimeout, o.pollInterval
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}
	if interval == 0 {
		interval = defaultPollInterval
	}
	return timeout, interval
}

// poll runs check every interval until it succeeds, the container
// stops, or the timeout elapses. When checkRunning is true the poll
// also probes target.Running between checks and fails fast once the
// container stopped; strategies whose check itself talks to the
// container (ForExec) pass false and rely on the final classification
// below.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error, checkRunning bool, values ...string) error {
	timeout, interval := o.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	var lastStateCheck time.Time
	for {
		if err := check(ctx); err != nil {
			var fatal fatalCheckError
			if errors.As(err, &fatal) {
				// The check could not run at all; retrying cannot
				// help, so surface the error right away.
				return safeDiagnosticError(fmt.Errorf("%s: %w", what, fatal.err), values...)
			}
			if ctx.Err() == nil {
				lastErr = err
			}
		} else {
			return nil
		}

		if checkRunning && time.Since(lastStateCheck) >= stateCheckInterval {
			lastStateCheck = time.Now()
			if running, err := target.Running(ctx); err == nil && !running {
				if lastErr == nil {
					return safeDiagnosticError(fmt.Errorf("%s: container stopped while waiting (last error: none)", what), values...)
				}
				return safeDiagnosticError(fmt.Errorf("%s: container stopped while waiting (last error: %w)", what, lastErr), values...)
			}
		}

		select {
		case <-ctx.Done():
			// Termination point: classify once. A poll without
			// state checks only inspects the container now, so a
			// stopped container is still reported accurately.
			// Probe only after our wait deadline; never override
			// caller cancellation, and bound the probe so a hung
			// backend cannot outlive the wait by queryTimeout.
			if !checkRunning && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), stateCheckInterval)
				running, err := target.Running(probeCtx)
				probeCancel()
				if err == nil && !running {
					if lastErr == nil {
						return safeDiagnosticError(fmt.Errorf("%s: container stopped while waiting (last error: none)", what), values...)
					}
					return safeDiagnosticError(fmt.Errorf("%s: container stopped while waiting (last error: %w)", what, lastErr), values...)
				}
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				if lastErr == nil {
					return safeDiagnosticError(fmt.Errorf("%s: %w (last error: none)", what, context.Canceled), values...)
				}
				return safeDiagnosticError(fmt.Errorf("%s: %w (last error: %w)", what, context.Canceled, lastErr), values...)
			}
			if lastErr == nil {
				return safeDiagnosticError(fmt.Errorf("%s: timed out after %v (last error: none)", what, timeout), values...)
			}
			return safeDiagnosticError(fmt.Errorf("%s: timed out after %v (last error: %w)", what, timeout, lastErr), values...)
		case <-time.After(interval):
		}
	}
}

// fatalCheckError wraps a check error that must end the poll
// immediately instead of being retried: the check could not run at all
// (CLI launch failure, unknown container), so retrying cannot help.
type fatalCheckError struct{ err error }

func (e fatalCheckError) Error() string { return e.err.Error() }
func (e fatalCheckError) Unwrap() error { return e.err }
