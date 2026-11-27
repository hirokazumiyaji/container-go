package wait

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// ExitStatus is a one-shot container's process result reported by an
// ExitCodeTarget. Its zero value means the target observed a state other than
// stopped.
type ExitStatus struct {
	// State is the container's lifecycle state at the observation.
	State State
	// Reported reports whether the backend supplied Code. It is meaningful
	// only when State is StateStopped.
	Reported bool
	// Code is the process exit code. It is meaningful only when Reported is
	// true.
	Code int
}

// ExitCodeTarget is an optional extension to Target for targets that can
// report a one-shot container's completion. ForExit returns a *ConfigError for
// targets that do not implement it.
type ExitCodeTarget interface {
	// ExitStatus reports the container's lifecycle state and, once it has
	// stopped, the backend-reported exit code. A backend that omits the exit
	// code reports StateStopped with Reported false.
	ExitStatus(ctx context.Context) (ExitStatus, error)
}

// ExitStrategy waits until a one-shot container stops with an accepted exit
// code.
type ExitStrategy struct {
	options
	exitCode    int
	exitCodeSet bool
	exitMatcher func(int) bool
	matcherSet  bool
}

// ForExit waits for a one-shot container to stop and accepts exit code 0 as
// success. Any other exit code fails the wait immediately. It is intended for
// job-like containers (database migrations, seeders, configuration
// generators) whose completion is the readiness signal, unlike the other
// strategies, which treat a stopped container as a lifecycle failure. Use
// WithExitCode or WithExitCodeMatcher to accept a different result.
//
// ForExit needs a target implementing the optional ExitCodeTarget interface;
// the container adapter does. A custom target without it fails immediately
// with a *ConfigError. Use ForExit directly, not inside ForAll or ForAny:
// those composites require the container to be running after every child
// succeeds.
func ForExit() *ExitStrategy {
	return &ExitStrategy{}
}

// clone returns a copy of the strategy. Builders derive a new value from the
// receiver so a strategy shared by parallel callers cannot be reconfigured
// through another caller's options.
func (s *ExitStrategy) clone() *ExitStrategy {
	cloned := *s
	return &cloned
}

// WithExitCode accepts exactly code and fails any other result immediately.
func (s *ExitStrategy) WithExitCode(code int) *ExitStrategy {
	cloned := s.clone()
	cloned.exitCode, cloned.exitCodeSet = code, true
	cloned.exitMatcher, cloned.matcherSet = nil, false
	return cloned
}

// WithExitCodeMatcher replaces the default exit-code-zero check. It also
// clears a fixed code set by WithExitCode.
func (s *ExitStrategy) WithExitCodeMatcher(matcher func(code int) bool) *ExitStrategy {
	cloned := s.clone()
	cloned.exitMatcher, cloned.matcherSet = matcher, true
	cloned.exitCode, cloned.exitCodeSet = 0, false
	return cloned
}

func (s *ExitStrategy) WithStartupTimeout(d time.Duration) *ExitStrategy {
	cloned := s.clone()
	cloned.startupTimeout = d
	return cloned
}

func (s *ExitStrategy) WithPollInterval(d time.Duration) *ExitStrategy {
	cloned := s.clone()
	cloned.pollInterval = d
	return cloned
}

func (s *ExitStrategy) validate() error {
	if err := s.options.validate("ForExit"); err != nil {
		return err
	}
	if s.exitCodeSet && s.exitCode < 0 {
		return &ConfigError{
			Strategy: "ForExit",
			Field:    "exit code",
			Value:    strconv.Itoa(s.exitCode),
			Reason:   "exit code must not be negative",
		}
	}
	if s.matcherSet && s.exitMatcher == nil {
		return &ConfigError{
			Strategy: "ForExit",
			Field:    "exit code matcher",
			Reason:   "exit code matcher must not be nil",
		}
	}
	return nil
}

func (s *ExitStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeDiagnosticError(retErr) }()
	if err := s.validate(); err != nil {
		return err
	}
	exitTarget, ok := target.(ExitCodeTarget)
	if !ok {
		return &ConfigError{
			Strategy: "ForExit",
			Field:    "target",
			Value:    fmt.Sprintf("%T", target),
			Reason:   "target does not report container exit status",
		}
	}
	matcher := s.exitMatcher
	switch {
	case matcher != nil:
	case s.exitCodeSet:
		want := s.exitCode
		matcher = func(code int) bool { return code == want }
	default:
		matcher = func(code int) bool { return code == 0 }
	}
	timeout, interval := s.effective()
	return waitForExit(ctx, exitTarget, matcher, timeout, interval)
}

// waitForExit polls completion instead of readiness: a stopped container is
// the success condition, so the generic poll lifecycle check (which requires
// the container to stay running) does not apply.
func waitForExit(ctx context.Context, target ExitCodeTarget, accept func(int) bool, timeout, interval time.Duration) error {
	const what = "wait for container exit"
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, lastErr, nil)
	}
	for {
		if err := terminationErr(); err != nil {
			return err
		}
		status, err := target.ExitStatus(waitCtx)
		if terminalErr := terminationErr(); terminalErr != nil {
			return terminalErr
		}
		if err != nil {
			if filtered := nonContextCause(err); filtered != nil {
				lastErr = filtered
			}
			if isPermanentCheckError(err) || permanentProbeError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered)
				}
			}
		} else {
			state := canonicalState(status.State)
			switch {
			case state == StateStopped:
				if !status.Reported {
					return fmt.Errorf("%s: container stopped but the backend did not report an exit code", what)
				}
				if accept(status.Code) {
					return nil
				}
				return fmt.Errorf("%s: container exited with code %d", what, status.Code)
			case terminalWaitState(state):
				return waitStoppedStateError(what, state, lastErr, nil)
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return terminationErrOrTimeout(terminationErr(), what, timeout, lastErr, nil)
		case <-timer.C:
		}
	}
}
