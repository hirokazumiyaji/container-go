package wait

import (
	"context"
	"time"
)

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	if err := o.validate(""); err != nil {
		return err
	}
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// lastErr and lastProbeErr hold structured non-context views. A newer
	// context-only observation never erases an already retained backend
	// cause, and a backend cause never contributes a context match.
	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr, lastProbeErr)
	}

	if err := terminationErr(); err != nil {
		return err
	}

	initialProbeCtx, initialProbeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
	state, err := targetState(initialProbeCtx, target)
	initialProbeErr := initialProbeCtx.Err()
	initialProbeCancel()
	if initialProbeErr != nil {
		err = joinNonNil(err, initialProbeErr)
	}
	if terminalErr := terminationErr(); terminalErr != nil {
		return terminalErr
	}
	if err != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			cause := permanentCause(err)
			if filtered := nonContextCause(cause); filtered != nil {
				return wrapWaitCause(what, filtered, lastProbeErr)
			}
			// Context-only permanent wrapper: continue polling rather than
			// returning an error that still matches context.Canceled.
		} else if filtered := nonContextCause(err); filtered != nil {
			lastProbeErr = filtered
		}
	} else if terminalWaitState(state) {
		return waitStoppedStateError(what, state, lastErr, lastProbeErr)
	}
	lastStateCheck = time.Now()

	probeState := func() error {
		if time.Since(lastStateCheck) < stateCheckInterval || waitCtx.Err() != nil {
			return nil //nolint:nilerr // throttled or cancelled probe: no state error to report
		}
		lastStateCheck = time.Now()
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		state, err := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			err = joinNonNil(err, probeCtxErr)
		}
		if err != nil {
			if terminalErr := terminationErr(); terminalErr != nil {
				if filtered := nonContextCause(err); filtered != nil {
					lastProbeErr = filtered
				}
				return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr, lastProbeErr)
			}
			if isPermanentCheckError(err) || permanentProbeError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, lastErr, lastProbeErr)
				}
				// Context-only permanent wrapper: keep polling.
			} else if filtered := nonContextCause(err); filtered != nil {
				lastProbeErr = filtered
			}
			return nil
		}
		if terminalErr := terminationErr(); terminalErr != nil {
			return terminalErr
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, lastErr, lastProbeErr)
		}
		return nil
	}

	for {
		if err := terminationErr(); err != nil {
			return err
		}
		if o.stateFirst {
			if err := probeState(); err != nil {
				return err
			}
		}

		if err := check(waitCtx); err != nil {
			// Keep the latest non-context check cause even when it races with
			// the context becoming done; the terminal classification must not
			// erase a typed backend error, and a mixed context branch must
			// not survive in a later timeout diagnostic.
			if filtered := nonContextCause(err); filtered != nil {
				lastErr = filtered
			}
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, lastErr, lastProbeErr)
				}
				// Context-only permanent wrapper while the wait is still
				// active: treat as transient and keep polling.
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, lastErr, lastProbeErr); err != nil {
				return err
			}
			return terminationErr()
		}

		if err := terminationErr(); err != nil {
			return err
		}
		if !o.stateFirst {
			if err := probeState(); err != nil {
				return err
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			return terminationErrOrTimeout(terminationErr(), what, timeout, lastErr, lastProbeErr)
		case <-timer.C:
		}
	}
}
