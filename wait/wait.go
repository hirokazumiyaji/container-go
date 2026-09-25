// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.
package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultStartupTimeout = 60 * time.Second
	defaultPollInterval   = 100 * time.Millisecond
	// stateCheckInterval bounds lifecycle probes during an ongoing
	// readiness poll. Stream failures are reclassified immediately.
	stateCheckInterval = time.Second
)

// ErrTargetNotFound can wrap a Target method error when the target no
// longer exists. Readiness polling treats it as permanent and returns
// immediately instead of retrying until its deadline.
var ErrTargetNotFound = errors.New("wait target not found")

// State is a container lifecycle state reported by a StateTarget.
type State string

const (
	StateUnknown    State = "unknown"
	StateCreated    State = "created"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateRestarting State = "restarting"
	StatePaused     State = "paused"
)

// Target is the container surface strategies probe. Running remains on
// the interface for compatibility with existing custom strategies.
// Targets that can distinguish startup transitions should also implement
// StateTarget; built-in strategies use State when available and fall back
// to Running otherwise. *container.Container is adapted by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port means the first declared
	// port.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is running. A false result
	// is terminal for a startup wait. Transient lifecycle states need the
	// richer StateTarget interface.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// StateTarget is the optional richer lifecycle surface used by built-in
// readiness strategies. State should return StateUnknown with an error when
// inspection fails. Permanent target disappearance should wrap
// ErrTargetNotFound; all other state errors are retried until the wait ends.
type StateTarget interface {
	Target
	State(ctx context.Context) (State, error)
}

// Strategy waits until a started container is ready for use. Composite
// strategies may run a strategy in a goroutine, so implementations should
// return when ctx is done.
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

// waitError keeps the context cause as the conventional Unwrap result while
// exposing transient check/state causes through errors.Is and errors.As.
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

func waitTimeoutError(what string, timeout time.Duration, lastErr error) error {
	message := fmt.Sprintf("%s: timed out after %v", what, timeout)
	if lastErr != nil {
		message += diagnosticSuffix(lastErr, nil)
	}
	return newWaitError(message, context.DeadlineExceeded, lastErr)
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

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	timeout, interval := o.effective()
	callerCtx := ctx
	startupDeadline := time.Now().Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, startupDeadline)
	defer cancel()

	var lastCheckErr, lastStateErr error
	if err := callerCtx.Err(); err != nil {
		return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
	}
	state, err := targetState(waitCtx, target)
	if err != nil {
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if waitCtx.Err() == nil {
			lastStateErr = err
		}
	} else if terminalWaitState(state) {
		if callerErr := callerCtx.Err(); callerErr != nil {
			return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
		}
		return stateFailure(what, state, lastCheckErr, lastStateErr)
	}
	lastStateCheck := time.Now()

	for {
		if err := waitCtx.Err(); err != nil {
			return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
		}

		err := check(waitCtx)
		if err == nil {
			if waitErr := waitCtx.Err(); waitErr != nil {
				return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
			}
			return nil
		}
		// Keep the check cause even when the context ends while the check
		// is returning; it is useful diagnostic context for the terminal
		// cancellation/deadline error.
		lastCheckErr = err
		if waitErr := waitCtx.Err(); waitErr != nil {
			return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
		}
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}

		if waitCtx.Err() == nil && time.Since(lastStateCheck) >= stateCheckInterval {
			lastStateCheck = time.Now()
			state, stateErr := targetState(waitCtx, target)
			if stateErr != nil {
				if permanentProbeError(stateErr) {
					return fmt.Errorf("%s: %w", what, stateErr)
				}
				lastStateErr = stateErr
			} else if waitCtx.Err() != nil {
				return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
			} else if terminalWaitState(state) {
				if callerErr := callerCtx.Err(); callerErr != nil {
					return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
				}
				return stateFailure(what, state, lastCheckErr, lastStateErr)
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// A final classification is useful only when this strategy's
			// own startup deadline won. Never detach from or probe after a
			// caller cancellation/deadline.
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && !callerDeadlineWins(callerCtx, startupDeadline) {
				probeCtx, probeCancel := context.WithTimeout(callerCtx, stateCheckInterval)
				if probeCtx.Err() == nil {
					state, stateErr := targetState(probeCtx, target)
					probeCancel()
					if callerErr := callerCtx.Err(); callerErr != nil {
						return newWaitError(fmt.Sprintf("%s: %s", what, callerErr)+diagnosticSuffix(lastCheckErr, stateErr), callerErr, lastCheckErr, stateErr)
					}
					if stateErr != nil {
						if permanentProbeError(stateErr) {
							return fmt.Errorf("%s: %w", what, stateErr)
						}
						lastStateErr = stateErr
					} else if terminalWaitState(state) {
						return stateFailure(what, state, lastCheckErr, lastStateErr)
					}
				} else {
					probeCancel()
				}
			}
			return pollTerminationError(callerCtx, waitCtx, startupDeadline, what, timeout, lastCheckErr, lastStateErr)
		case <-timer.C:
		}
	}
}

// pollTerminationError classifies the context that ended a leaf poll. The
// caller is checked first, and the startup deadline is only reported as the
// strategy timeout when the caller did not win the race.
func pollTerminationError(callerCtx, waitCtx context.Context, startupDeadline time.Time, what string, timeout time.Duration, checkErr, stateErr error) error {
	if err := callerCtx.Err(); err != nil {
		message := fmt.Sprintf("%s: %s", what, err)
		return newWaitError(message+diagnosticSuffix(checkErr, stateErr), err, checkErr, stateErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callerDeadlineWins(callerCtx, startupDeadline) {
			message := fmt.Sprintf("%s: caller deadline exceeded", what)
			return newWaitError(message+diagnosticSuffix(checkErr, stateErr), context.DeadlineExceeded, checkErr, stateErr)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, joinNonNil(checkErr, stateErr))
		}
		message := fmt.Sprintf("%s: %s", what, err)
		return newWaitError(message+diagnosticSuffix(checkErr, stateErr), err, checkErr, stateErr)
	}
	return waitTimeoutError(what, timeout, joinNonNil(checkErr, stateErr))
}

// callerDeadlineWins reports whether the caller's configured deadline is at
// or before this strategy's own deadline. Checking the timestamp as well as
// Err closes the small race where a custom context has not published Err yet.
func callerDeadlineWins(callerCtx context.Context, startupDeadline time.Time) bool {
	if callerCtx.Err() != nil {
		return true
	}
	deadline, ok := callerCtx.Deadline()
	return ok && !deadline.After(startupDeadline)
}

// targetState uses the richer optional interface when available. Running is a
// compatibility fallback whose false result necessarily means stopped.
func targetState(ctx context.Context, target Target) (State, error) {
	if stateTarget, ok := target.(StateTarget); ok {
		state, err := stateTarget.State(ctx)
		if err != nil {
			return StateUnknown, err
		}
		return canonicalState(state), nil
	}
	running, err := target.Running(ctx)
	if err != nil {
		return StateUnknown, err
	}
	if running {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func canonicalState(state State) State {
	switch state {
	case StateCreated, StateRunning, StateStopping, StateStopped, StateRestarting, StatePaused:
		return state
	default:
		return StateUnknown
	}
}

// terminalWaitState reports lifecycle states from which a container cannot
// become ready during startup without external intervention. Created,
// restarting, unknown, and transient inspect errors are retried under the
// startup timeout. Stopping includes backend removal transitions.
func terminalWaitState(state State) bool {
	return state == StateStopping || state == StateStopped || state == StatePaused
}

func stateFailure(what string, state State, checkErr, stateErr error) error {
	message := fmt.Sprintf("%s: container %s while waiting", what, state)
	return newWaitError(message+diagnosticSuffix(checkErr, stateErr), checkErr, stateErr)
}

func diagnosticSuffix(checkErr, stateErr error) string {
	var causes []string
	if checkErr != nil {
		causes = append(causes, fmt.Sprintf("last check error: %v", checkErr))
	}
	if stateErr != nil {
		causes = append(causes, fmt.Sprintf("last state error: %v", stateErr))
	}
	if len(causes) == 0 {
		return ""
	}
	return " (" + strings.Join(causes, "; ") + ")"
}

// permanentProbeError identifies failures that cannot recover by retrying the
// same target operation. Backend adapters should wrap disappearance in
// ErrTargetNotFound. Launch failures are permanent; ordinary CLI/inspect
// failures remain retryable unless the operation-specific strategy says
// otherwise.
func permanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTargetNotFound) {
		return true
	}
	var launchErr *exec.Error
	return errors.As(err, &launchErr)
}
