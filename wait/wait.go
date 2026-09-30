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

// Strategy waits until a started container is ready for use. Implementations
// must return when ctx is done so composite strategies can collect their final
// error without extending the caller's cancellation contract indefinitely.
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

type waitError struct {
	message string
	primary error
	causes  []error
}

func (e *waitError) Error() string { return e.message }

// Unwrap keeps the context error as the conventional single cause for
// callers that use errors.Unwrap. Is and As below expose every additional
// cause without changing that compatibility.
func (e *waitError) Unwrap() error { return e.primary }

func (e *waitError) Is(target error) bool {
	for _, cause := range e.causes {
		if errors.Is(cause, target) {
			return true
		}
	}
	return false
}

func (e *waitError) As(target any) bool {
	for _, cause := range e.causes {
		if errors.As(cause, target) {
			return true
		}
	}
	return false
}

func newWaitError(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	var primary error
	if len(filtered) > 0 {
		primary = filtered[0]
	}
	return &waitError{message: message, primary: primary, causes: filtered}
}

func waitContextError(what string, contextErr error, lastErr error) error {
	message := fmt.Sprintf("%s: %s", what, contextErr)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, contextErr, lastErr)
}

func waitTimeoutError(what string, timeout time.Duration, lastErr error) error {
	message := fmt.Sprintf("%s: timed out after %v", what, timeout)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, context.DeadlineExceeded, lastErr)
}

func waitStoppedError(what string, lastErr error) error {
	message := fmt.Sprintf("%s: container stopped while waiting", what)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, lastErr)
}

// waitContextTerminationError classifies the context that ended a wait. The
// caller's context is checked first so a caller deadline is not reported
// as the strategy's own startup timeout.
func waitContextTerminationError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, lastErr error) error {
	if err := callerCtx.Err(); err != nil {
		return waitContextError(what, err, lastErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, lastErr)
		}
		return waitContextError(what, err, lastErr)
	}
	return nil
}

// poll runs check every interval until it succeeds, the container
// stops, or the timeout elapses. When checkRunning is true the poll
// also probes target.Running between checks and fails fast once the
// container stopped; strategies whose check itself talks to the
// container (ForExec) pass false and rely on the final classification
// below.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error, checkRunning bool) error {
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr)
	}
	for {
		if err := terminationErr(); err != nil {
			return err
		}

		if err := check(waitCtx); err != nil {
			// The check can return a backend error at the same moment
			// its context ends. Keep that error as the latest cause
			// before classifying the terminal context.
			lastErr = err

			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}

			var fatal fatalCheckError
			if errors.As(err, &fatal) {
				// The check could not run at all; retrying cannot
				// help, so surface the error right away.
				return fmt.Errorf("%s: %w", what, fatal.err)
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			return nil
		}

		if checkRunning && time.Since(lastStateCheck) >= stateCheckInterval {
			lastStateCheck = time.Now()
			running, err := target.Running(waitCtx)
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if err == nil && !running {
				return waitStoppedError(what, lastErr)
			}
		}

		select {
		case <-waitCtx.Done():
			// A poll without state checks performs one final
			// classification only after its own startup timeout. It
			// must not start a probe after the caller's context ended,
			// and the probe retains caller cancellation as a bound.
			if !checkRunning && callerCtx.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				probeCtx, probeCancel := context.WithTimeout(callerCtx, stateCheckInterval)
				if probeErr := probeCtx.Err(); probeErr != nil {
					probeCancel()
					return waitContextError(what, probeErr, lastErr)
				}
				running, err := target.Running(probeCtx)
				probeCancel()
				if callerErr := callerCtx.Err(); callerErr != nil {
					return waitContextError(what, callerErr, lastErr)
				}
				if err == nil && !running {
					return waitStoppedError(what, lastErr)
				}
			}
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			// waitCtx.Done was observed, so this is only a defensive
			// fallback for unusual Context implementations.
			return waitTimeoutError(what, timeout, lastErr)
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
