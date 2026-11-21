package wait

import (
	"context"
	"errors"
	"time"
)

// FuncStrategy waits until a caller-provided function reports readiness.
type FuncStrategy struct {
	options
	fn        func(context.Context, Target) error
	exitCheck bool
}

// ForFunc waits until fn returns nil.
//
// fn runs once per poll with the wait context and the caller's target, so it
// can use any client to probe the started container (a gRPC health check, a
// cache PING, a broker metadata call, and so on). A non-nil error is retried
// on the next poll, and the latest error is reported when the startup timeout
// expires.
//
// An error matching context.Canceled ends the wait immediately even while the
// wait context is still live. Because the wait was not canceled, that
// termination is reported as a function failure rather than as caller
// cancellation. Other context-shaped errors, such as a per-attempt deadline
// from an HTTP client, are retried like any other probe failure. An error this
// package classifies as permanent (for example one wrapping
// ErrInvalidConfiguration, ErrPortNotExposed, ErrContainerNotFound,
// ErrTargetNotFound, or ErrLogStreamSetup) also ends the wait immediately
// instead of being retried.
//
// The default startup timeout is 60s and the default poll interval is 100ms,
// matching the connection-based strategies. A stopped, stopping, or paused
// container fails the wait; use WithExitCheck(false) when fn intentionally
// observes container exit.
func ForFunc(fn func(ctx context.Context, target Target) error) *FuncStrategy {
	return &FuncStrategy{fn: fn, exitCheck: true}
}

// clone returns a copy of the strategy. Builders derive a new value from the
// receiver so a strategy shared by parallel callers cannot be reconfigured
// through another caller's options.
func (s *FuncStrategy) clone() *FuncStrategy {
	cloned := *s
	return &cloned
}

// WithExitCheck controls the lifecycle guard that ends the wait when the
// container stops. It is enabled by default. Disabling it is useful when fn
// waits for the container to exit on its own; the callback still receives the
// caller's target and can inspect its real state, while the wait then ends
// only through a callback result or the startup timeout.
//
// The opt-out applies to this ForFunc's own poll. ForAll and ForAny run their
// own lifecycle monitor and final lifecycle check against the real target, so
// a composite still fails when the container exits. Use a ForFunc that waits
// for container exit outside a composite strategy.
func (s *FuncStrategy) WithExitCheck(enabled bool) *FuncStrategy {
	cloned := s.clone()
	cloned.exitCheck = enabled
	return cloned
}

func (s *FuncStrategy) WithStartupTimeout(d time.Duration) *FuncStrategy {
	cloned := s.clone()
	cloned.startupTimeout = d
	return cloned
}

func (s *FuncStrategy) WithPollInterval(d time.Duration) *FuncStrategy {
	cloned := s.clone()
	cloned.pollInterval = d
	return cloned
}

func (s *FuncStrategy) validate() error {
	if err := s.options.validate("ForFunc"); err != nil {
		return err
	}
	if s.fn == nil {
		return &ConfigError{
			Strategy: "ForFunc",
			Field:    "function",
			Reason:   "function must not be nil",
		}
	}
	return nil
}

func (s *FuncStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	defer func() { retErr = safeStrategyError(retErr, s) }()
	if err := s.validate(); err != nil {
		return err
	}
	pollTarget := target
	if !s.exitCheck {
		pollTarget = noExitCheckTarget{Target: target}
	}
	return poll(ctx, s.options, pollTarget, "wait for custom function", func(ctx context.Context) error {
		err := s.fn(ctx, target)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !errors.Is(err, context.Canceled) {
			return err
		}
		// The callback asked to stop while the wait context is still live.
		// Keep any independent backend cause, but never let the context
		// branch terminate the poll as a caller cancellation.
		if cause := nonContextCause(err); cause != nil {
			return fatalCheckError{err: cause}
		}
		return fatalCheckError{err: funcStopError{}}
	})
}

// noExitCheckTarget keeps poll's lifecycle guard from ending a ForFunc wait
// that disabled the exit check. The callback still receives the caller's
// target, so it can observe the real state itself.
type noExitCheckTarget struct{ Target }

func (noExitCheckTarget) Running(context.Context) (bool, error) { return true, nil }

func (noExitCheckTarget) State(context.Context) (State, error) { return StateRunning, nil }

// funcStopError ends a ForFunc wait when the callback reports an ended
// context while the wait itself is still live. It deliberately does not
// unwrap that sentinel: the wait context was not canceled, and a caller-visible
// context match would misreport the wait as interrupted.
type funcStopError struct{}

func (funcStopError) Error() string {
	return "custom wait function returned a context cancellation"
}
