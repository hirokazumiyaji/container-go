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

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	defaultStartupTimeout      = 60 * time.Second
	defaultPollInterval        = 100 * time.Millisecond
	stateCheckInterval         = time.Second
	lifecycleProbeTimeout      = 5 * time.Second
	finalLifecycleProbeTimeout = 30 * time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port asks the built-in adapter for
	// its first exposed TCP declaration, falling back to a published TCP
	// declaration.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream. A
	// terminal CLI failure is returned by Read after the stream starts.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// State is a container lifecycle state reported by Target.
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

// StateTarget is an optional extension to Target for targets that can
// distinguish transitional states (Created, Restarting, Unknown) from
// terminal ones (Stopped, Stopping, Paused). Built-in strategies use it
// when available and fall back to Running.
type StateTarget interface {
	State(ctx context.Context) (State, error)
}

// Strategy waits until a started container is ready for use.
// Implementations must return when ctx is done so composite strategies can
// collect their final causes without extending the caller's budget.
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

// waitError keeps a conventional single context cause for errors.Unwrap
// while exposing additional transient causes through errors.Is/As.
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

func waitStoppedStateError(what string, state State, lastErr error) error {
	message := fmt.Sprintf("%s: container %s while waiting", what, state)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, lastErr)
}

func stateFailure(what string, state State, checkErr, stateErr error) error {
	return waitStoppedStateError(what, state, joinNonNil(checkErr, stateErr))
}

// targetState uses the richer optional interface when available. Running is a
// compatibility fallback whose false result necessarily means stopped.
func targetState(ctx context.Context, target Target) (State, error) {
	if stateTarget, ok := target.(StateTarget); ok {
		state, err := stateTarget.State(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if permanentProbeError(err) || isPermanentCheckError(err) {
				return StateUnknown, ctxErr
			}
			return StateUnknown, joinNonNil(err, ctxErr)
		}
		if err != nil {
			return StateUnknown, err
		}
		return canonicalState(state), nil
	}
	running, err := target.Running(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			return StateUnknown, ctxErr
		}
		return StateUnknown, joinNonNil(err, ctxErr)
	}
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

func finalLifecycleError(
	callerCtx, waitCtx context.Context,
	target Target,
	what string,
	timeout, interval time.Duration,
	lastErr error,
) error {
	// A successful endpoint/exec/log check can race a short-lived daemon
	// hiccup. The lifecycle check is still a fail-fast guard for a stopped
	// container, but transient probe failures get another attempt while the
	// original wait budget remains. Retries reuse the strategy's poll
	// interval so a transient daemon failure cannot turn into a faster
	// probe loop than the check it follows, and only the latest probe cause
	// is retained so retries cannot grow an error tree.
	var lastProbeErr error
	for {
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			return waitContextTerminationError(
				callerCtx,
				waitCtx,
				what,
				timeout,
				joinNonNil(lastErr, lastProbeErr, err),
			)
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		if terminalErr := waitContextTerminationError(
			callerCtx,
			waitCtx,
			what,
			timeout,
			joinNonNil(lastErr, lastProbeErr, probeErr),
		); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			lastProbeErr = probeErr
			if isPermanentCheckError(probeErr) {
				return wrapWaitCause(what, permanentCause(probeErr), joinNonNil(lastErr, lastProbeErr))
			}
			if err := waitForReconnect(waitCtx, interval); err != nil {
				return waitContextTerminationError(
					callerCtx,
					waitCtx,
					what,
					timeout,
					joinNonNil(lastErr, lastProbeErr),
				)
			}
			continue
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
		}
		if state != StateRunning {
			return fmt.Errorf("%s: final lifecycle state %s; want running", what, state)
		}
		return nil
	}
}

func joinNonNil(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return errors.Join(filtered...)
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

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	if err := o.validate(); err != nil {
		return err
	}
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
	}

	if err := terminationErr(); err != nil {
		return err
	}

	state, err := targetState(waitCtx, target)
	if terminalErr := terminationErr(); terminalErr != nil {
		return terminalErr
	}
	if err != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			return wrapWaitCause(what, permanentCause(err), lastProbeErr)
		}
		lastProbeErr = err
	} else if terminalWaitState(state) {
		return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
	}
	lastStateCheck = time.Now()

	for {
		if err := terminationErr(); err != nil {
			return err
		}

		if err := check(waitCtx); err != nil {
			// Keep the latest check cause even when it races with the
			// context becoming done; the terminal classification must not
			// erase a typed backend error.
			lastErr = err
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) {
				return wrapWaitCause(what, permanentCause(err), joinNonNil(lastErr, lastProbeErr))
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			causes := joinNonNil(lastErr, lastProbeErr)
			if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, causes); err != nil {
				return err
			}
			return terminationErr()
		}

		if err := terminationErr(); err != nil {
			return err
		}
		if time.Since(lastStateCheck) >= stateCheckInterval && waitCtx.Err() == nil {
			lastStateCheck = time.Now()
			state, err := targetState(waitCtx, target)
			if err != nil {
				if terminalErr := terminationErr(); terminalErr != nil {
					lastProbeErr = err
					return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
				}
				if isPermanentCheckError(err) || permanentProbeError(err) {
					return wrapWaitCause(what, permanentCause(err), joinNonNil(lastErr, lastProbeErr))
				}
				lastProbeErr = err
			} else if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			} else if terminalWaitState(state) {
				return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			return terminationErrOrTimeout(terminationErr(), what, timeout, joinNonNil(lastErr, lastProbeErr))
		case <-timer.C:
		}
	}
}

func terminationErrOrTimeout(err error, what string, timeout time.Duration, lastErr error) error {
	if err != nil {
		return err
	}
	return waitTimeoutError(what, timeout, lastErr)
}

func permanentCause(err error) error {
	var fatal fatalCheckError
	if errors.As(err, &fatal) && fatal.err != nil {
		return fatal.err
	}
	var fatalPointer *fatalCheckError
	if errors.As(err, &fatalPointer) && fatalPointer != nil && fatalPointer.err != nil {
		return fatalPointer.err
	}
	return err
}

// fatalCheckError wraps a check error that must end the poll
// immediately instead of being retried: the check cannot succeed as
// configured (for example, a CLI launch failure, unknown container, or
// invalid request), so retrying cannot help.
type fatalCheckError struct{ err error }

func (e fatalCheckError) Error() string {
	if e.err == nil {
		return "permanent wait error"
	}
	return e.err.Error()
}
func (e fatalCheckError) Unwrap() error { return e.err }

func permanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTargetNotFound) {
		return true
	}
	return cli.PermanentStartError(err)
}
