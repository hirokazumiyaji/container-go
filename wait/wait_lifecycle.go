package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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

func waitContextTerminationError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, checkErr, probeErr error) error {
	if err := callerCtx.Err(); err != nil {
		return waitContextError(what, err, checkErr, probeErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, checkErr, probeErr)
		}
		return waitContextError(what, err, checkErr, probeErr)
	}
	return nil
}

func finalLifecycleError(
	callerCtx, waitCtx context.Context,
	target Target,
	what string,
	timeout, interval time.Duration,
	checkErr, probeErr error,
) error {
	// A successful endpoint/exec/log check can race a short-lived daemon
	// hiccup. The lifecycle check is still a fail-fast guard for a stopped
	// container, but transient probe failures get another attempt while the
	// original wait budget remains. Retries reuse the strategy's poll
	// interval so a transient daemon failure cannot turn into a faster
	// probe loop than the check it follows, and only the latest probe cause
	// is retained so retries cannot grow an error tree.
	lastProbeErr := probeErr
	for {
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			// The probe context is derived from waitCtx, so a done probe
			// context means the caller or startup budget ended. probeCtx.Err()
			// itself is not a cause and is deliberately not retained.
			return waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr)
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		// Retain only non-context branches for diagnostics/timeout wrapping,
		// but keep the raw probeErr for control flow so a pure probe-context
		// deadline still retries instead of falling through as StateUnknown.
		if filtered := nonContextCause(probeErr); filtered != nil {
			lastProbeErr = filtered
		}
		if terminalErr := waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			if isPermanentCheckError(probeErr) {
				cause := permanentCause(probeErr)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, checkErr, lastProbeErr)
				}
				// Context-only permanent wrapper while the wait is still
				// active: always retry. A prior transient lastProbeErr is
				// retained only for a later timeout diagnostic, not as an
				// immediate failure of an otherwise ready target.
			}
			if err := waitForReconnect(waitCtx, interval); err != nil {
				return waitContextTerminationError(callerCtx, waitCtx, what, timeout, checkErr, lastProbeErr)
			}
			continue
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, checkErr, lastProbeErr)
		}
		if state != StateRunning {
			return fmt.Errorf("%s: final lifecycle state %s; want running", what, state)
		}
		return nil
	}
}
