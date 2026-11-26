package wait

import (
	"context"
	"errors"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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

	state, err := targetState(waitCtx, target)
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
			return nil
		}
		lastStateCheck = time.Now()
		state, err := targetState(waitCtx, target)
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

func terminationErrOrTimeout(err error, what string, timeout time.Duration, checkErr, probeErr error) error {
	if err != nil {
		return err
	}
	return waitTimeoutError(what, timeout, checkErr, probeErr)
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
