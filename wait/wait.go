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
	// spawns a CLI process while polling. A final probe is reserved
	// inside the same wait budget and is never started after it expires.
	stateCheckInterval = time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared port ("6379/tcp" or "6379") to an
	// addressable host/port. An empty port selects the first declared
	// TCP port.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs returns a stream. Close releases the stream.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command inside the container.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// Strategy waits until a started container is ready for use.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

// waitError keeps a conventional single context cause for errors.Unwrap
// while exposing every additional cause through errors.Is and errors.As.
type waitError struct {
	message string
	primary error
	causes  []error
}

func (e *waitError) Error() string { return e.message }
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

func joinNonNil(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return errors.Join(filtered...)
	}
}

func waitContextError(what string, contextErr, lastErr error) error {
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

func wrapWaitCause(what string, cause error, previous ...error) error {
	if cause == nil {
		return nil
	}
	causes := make([]error, 0, len(previous)+1)
	causes = append(causes, cause)
	causes = append(causes, previous...)
	return newWaitError(fmt.Sprintf("%s: %v", what, cause), causes...)
}

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

// poll runs check every interval until it succeeds, the container stops,
// or the timeout elapses. When checkRunning is true the poll also probes
// target.Running between checks and fails fast once the container stopped;
// strategies whose check itself talks to the container (ForExec) reserve a
// final state probe inside the same wait budget.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error, checkRunning bool) error {
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	finalStateChecked := false
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
	}

	for {
		if err := terminationErr(); err != nil {
			return err
		}

		if err := check(waitCtx); err != nil {
			// Keep a typed check error even when the context completes while
			// the backend is returning it.
			lastErr = err
			if terminal := terminationErr(); terminal != nil {
				return terminal
			}
			var fatal fatalCheckError
			if errors.As(err, &fatal) {
				return wrapWaitCause(what, fatal.err, joinNonNil(lastErr, lastProbeErr))
			}
		} else {
			if terminal := terminationErr(); terminal != nil {
				return terminal
			}
			return nil
		}

		if terminal := terminationErr(); terminal != nil {
			return terminal
		}
		shouldCheckState := checkRunning && time.Since(lastStateCheck) >= stateCheckInterval
		if !checkRunning && !finalStateChecked {
			_, callerHasDeadline := callerCtx.Deadline()
			if deadline, ok := waitCtx.Deadline(); ok && !callerHasDeadline && time.Until(deadline) <= stateCheckInterval {
				shouldCheckState = true
				finalStateChecked = true
			}
		}
		if shouldCheckState && waitCtx.Err() == nil {
			lastStateCheck = time.Now()
			running, stateErr := target.Running(waitCtx)
			if stateErr != nil {
				lastProbeErr = stateErr
				if terminal := terminationErr(); terminal != nil {
					return terminal
				}
			} else if terminal := terminationErr(); terminal != nil {
				return terminal
			} else if !running {
				return waitStoppedError(what, joinNonNil(lastErr, lastProbeErr))
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			if terminal := terminationErr(); terminal != nil {
				return terminal
			}
			return waitTimeoutError(what, timeout, joinNonNil(lastErr, lastProbeErr))
		case <-timer.C:
		}
	}
}

type fatalCheckError struct{ err error }

func (e fatalCheckError) Error() string { return e.err.Error() }
func (e fatalCheckError) Unwrap() error { return e.err }
