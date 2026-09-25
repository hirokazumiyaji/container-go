package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// compositeDrainTimeout bounds collection of child results after a
// composite context ends; non-cooperative children cannot hold the caller.
const compositeDrainTimeout = 100 * time.Millisecond

// AllStrategy waits for every strategy, in order.
type AllStrategy struct {
	strategies     []Strategy
	startupTimeout time.Duration
}

// ForAll waits for every strategy, in order.
func ForAll(strategies ...Strategy) *AllStrategy {
	return &AllStrategy{strategies: strategies}
}

// WithStartupTimeout bounds the total time spent waiting across all
// strategies in the sequence. A non-positive d leaves the sequence
// unbounded, relying on each strategy's own startup timeout. Negative
// values retain the original composite-wait compatibility meaning.
func (s *AllStrategy) WithStartupTimeout(d time.Duration) *AllStrategy {
	s.startupTimeout = d
	return s
}

func (s *AllStrategy) validate() error {
	// Negative composite timeouts historically meant unbounded. Keep
	// that compatibility while still validating every child.
	return validateStrategies(s.strategies)
}

func (s *AllStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
	}

	callerCtx := ctx
	waitCtx := ctx
	var cancel context.CancelFunc
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		waitCtx, cancel = context.WithDeadline(ctx, startupDeadline)
		defer cancel()
	}
	callerDeadline, callerHasDeadline := callerCtx.Deadline()
	if err := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, "before waiting"); err != nil {
		return err
	}

	for i, strategy := range s.strategies {
		if err := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("before strategy %d", i)); err != nil {
			return err
		}
		err := strategy.WaitUntilReady(waitCtx, target)
		if err != nil {
			term := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("in strategy %d", i))
			if term == nil && s.startupTimeout > 0 && !time.Now().Before(startupDeadline) && !callerOwnsDeadline(callerDeadline, callerHasDeadline, startupDeadline) && errors.Is(err, context.DeadlineExceeded) && callerCtx.Err() == nil {
				// A child can observe the derived deadline just before the
				// parent context records Err. Treat that boundary as the
				// composite's startup timeout rather than leaking a generic
				// child deadline.
				term = newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed in strategy %d", s.startupTimeout, i), context.DeadlineExceeded)
			}
			if term != nil {
				primary := term
				if cause := errors.Unwrap(term); cause != nil {
					primary = cause
				}
				return newWaitError(fmt.Sprintf("wait for all: %v (strategy %d: %v)", term, i, err), primary, term, err)
			}
			return err
		}
		if err := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("after strategy %d", i)); err != nil {
			return err
		}
	}
	return nil
}

func allContextError(callerCtx, waitCtx context.Context, startupTimeout time.Duration, startupDeadline, callerDeadline time.Time, callerHasDeadline bool, phase string) error {
	if err := callerCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return newWaitError(fmt.Sprintf("wait for all: caller deadline elapsed %s", phase), err)
		}
		return newWaitError(fmt.Sprintf("wait for all: caller context ended %s", phase), err)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callerOwnsDeadline(callerDeadline, callerHasDeadline, startupDeadline) {
			return newWaitError(fmt.Sprintf("wait for all: caller deadline elapsed %s", phase), context.DeadlineExceeded)
		}
		if startupTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed %s", startupTimeout, phase), context.DeadlineExceeded)
		}
		return newWaitError(fmt.Sprintf("wait for all: context ended %s", phase), err)
	}
	return nil
}

func callerOwnsDeadline(callerDeadline time.Time, callerHasDeadline bool, startupDeadline time.Time) bool {
	return callerHasDeadline && !callerDeadline.After(startupDeadline)
}

// AnyStrategy waits until one of the strategies succeeds.
type AnyStrategy struct {
	strategies     []Strategy
	startupTimeout time.Duration
}

// ForAny waits until one of the strategies succeeds.
func ForAny(strategies ...Strategy) *AnyStrategy {
	return &AnyStrategy{strategies: strategies}
}

// WithStartupTimeout bounds the total time spent waiting for any
// strategy to succeed. A non-positive d leaves the wait unbounded,
// relying on each strategy's own startup timeout. Negative values
// retain the original composite-wait compatibility meaning.
func (s *AnyStrategy) WithStartupTimeout(d time.Duration) *AnyStrategy {
	s.startupTimeout = d
	return s
}

func (s *AnyStrategy) validate() error {
	// Negative composite timeouts historically meant unbounded. Keep
	// that compatibility while still validating every child.
	return validateStrategies(s.strategies)
}

func (s *AnyStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := s.validate(); err != nil {
		return err
	}
	callerCtx := ctx
	if err := callerCtx.Err(); err != nil {
		return newWaitError("wait for any: caller context ended before waiting", err)
	}
	if len(s.strategies) == 0 {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitCtx := runCtx
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		var timeoutCancel context.CancelFunc
		waitCtx, timeoutCancel = context.WithDeadline(runCtx, startupDeadline)
		defer timeoutCancel()
	}
	callerDeadline, callerHasDeadline := callerCtx.Deadline()

	results := make(chan error, len(s.strategies))
	for _, strategy := range s.strategies {
		go func(strategy Strategy) {
			results <- strategy.WaitUntilReady(waitCtx, target)
		}(strategy)
	}

	var errs []error
	remaining := len(s.strategies)
	for remaining > 0 {
		select {
		case err := <-results:
			remaining--
			if err != nil {
				errs = append(errs, err)
			}
			if terminal := anyContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline); terminal != nil {
				cancel()
				childErrs := drainCompositeResults(results, remaining)
				return errors.Join(append([]error{terminal}, append(errs, childErrs...)...)...)
			}
			if err == nil {
				return nil
			}
		case <-waitCtx.Done():
			terminalErr := anyContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline)
			if terminalErr == nil {
				// A custom Context may close Done without exposing Err.
				terminalErr = context.Canceled
			}
			cancel()
			childErrs := drainCompositeResults(results, remaining)
			return errors.Join(append([]error{terminalErr}, append(errs, childErrs...)...)...)
		}
	}
	return errors.Join(errs...)
}

func drainCompositeResults(results <-chan error, count int) []error {
	if count <= 0 {
		return nil
	}
	childErrs := make([]error, 0, count)
	timer := time.NewTimer(compositeDrainTimeout)
	defer timer.Stop()
	for received := 0; received < count; received++ {
		select {
		case err := <-results:
			if err != nil {
				childErrs = append(childErrs, err)
			}
		case <-timer.C:
			return childErrs
		}
	}
	return childErrs
}

func anyContextError(callerCtx, waitCtx context.Context, startupTimeout time.Duration, startupDeadline, callerDeadline time.Time, callerHasDeadline bool) error {
	if err := callerCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return newWaitError("wait for any: caller deadline elapsed", err)
		}
		return newWaitError("wait for any: caller context ended", err)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callerOwnsDeadline(callerDeadline, callerHasDeadline, startupDeadline) {
			return newWaitError("wait for any: caller deadline elapsed", context.DeadlineExceeded)
		}
		if startupTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			return newWaitError(fmt.Sprintf("wait for any: startup timeout %v elapsed", startupTimeout), context.DeadlineExceeded)
		}
		return newWaitError("wait for any: context ended", err)
	}
	return nil
}
