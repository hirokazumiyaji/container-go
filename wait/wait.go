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

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	timeout, interval := o.effective()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastCheckErr, lastStateErr error
	state, err := targetState(ctx, target)
	if err != nil {
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ctx.Err() == nil {
			lastStateErr = err
		}
	} else if terminalWaitState(state) {
		return stateFailure(what, state, lastCheckErr, lastStateErr)
	}
	lastStateCheck := time.Now()

	for {
		err := check(ctx)
		if err == nil {
			return nil
		}
		if permanentProbeError(err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if ctx.Err() == nil {
			lastCheckErr = err
		}

		if ctx.Err() == nil && time.Since(lastStateCheck) >= stateCheckInterval {
			lastStateCheck = time.Now()
			state, stateErr := targetState(ctx, target)
			if stateErr != nil {
				if permanentProbeError(stateErr) {
					return fmt.Errorf("%s: %w", what, stateErr)
				}
				lastStateErr = stateErr
			} else if terminalWaitState(state) {
				return stateFailure(what, state, lastCheckErr, lastStateErr)
			}
		}

		select {
		case <-ctx.Done():
			// Classify once after the startup deadline. Do not probe after
			// caller cancellation, and bound the detached probe so a hung
			// backend cannot consume another full query timeout.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), stateCheckInterval)
				state, stateErr := targetState(probeCtx, target)
				probeCancel()
				if stateErr != nil {
					if permanentProbeError(stateErr) {
						return fmt.Errorf("%s: %w", what, stateErr)
					}
					lastStateErr = stateErr
				} else if terminalWaitState(state) {
					return stateFailure(what, state, lastCheckErr, lastStateErr)
				}
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				return fmt.Errorf("%s: %w%s", what, context.Canceled, diagnosticSuffix(lastCheckErr, lastStateErr))
			}
			return fmt.Errorf("%s: timed out after %v%s", what, timeout, diagnosticSuffix(lastCheckErr, lastStateErr))
		case <-time.After(interval):
		}
	}
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
	return fmt.Errorf("%s: container %s while waiting%s", what, state, diagnosticSuffix(checkErr, stateErr))
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
// ErrTargetNotFound. CLI launch failures are also permanent.
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
