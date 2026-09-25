package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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
// unbounded, relying on each strategy's own startup timeout.
func (s *AllStrategy) WithStartupTimeout(d time.Duration) *AllStrategy {
	s.startupTimeout = d
	return s
}

func (s *AllStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	callerCtx := ctx
	waitCtx := ctx
	if s.startupTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, s.startupTimeout)
		defer cancel()
	}

	for i, strategy := range s.strategies {
		if err := callerCtx.Err(); err != nil {
			return newWaitError(fmt.Sprintf("wait for all: caller context ended before strategy %d ran", i), err)
		}
		if err := waitCtx.Err(); err != nil {
			if s.startupTimeout > 0 && callerCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed before strategy %d ran", s.startupTimeout, i), err)
			}
			return newWaitError(fmt.Sprintf("wait for all: context ended before strategy %d ran", i), err)
		}

		if err := strategy.WaitUntilReady(waitCtx, target); err != nil {
			if callerErr := callerCtx.Err(); callerErr != nil {
				return newWaitError(fmt.Sprintf("wait for all: strategy %d: %v", i, err), callerErr, err)
			}
			if s.startupTimeout > 0 && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed in strategy %d: %v", s.startupTimeout, i, err), waitCtx.Err(), err)
			}
			if waitErr := waitCtx.Err(); waitErr != nil {
				return newWaitError(fmt.Sprintf("wait for all: strategy %d: %v", i, err), waitErr, err)
			}
			return err
		}

		if err := callerCtx.Err(); err != nil {
			return newWaitError(fmt.Sprintf("wait for all: caller context ended after strategy %d", i), err)
		}
		if err := waitCtx.Err(); err != nil {
			if s.startupTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
				return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed after strategy %d", s.startupTimeout, i), err)
			}
			return newWaitError(fmt.Sprintf("wait for all: context ended after strategy %d", i), err)
		}
	}
	return nil
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
// relying on each strategy's own startup timeout.
func (s *AnyStrategy) WithStartupTimeout(d time.Duration) *AnyStrategy {
	s.startupTimeout = d
	return s
}

func (s *AnyStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if len(s.strategies) == 0 {
		return nil
	}
	callerCtx := ctx
	if err := callerCtx.Err(); err != nil {
		return newWaitError("wait for any: caller context ended", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitCtx := runCtx

	if s.startupTimeout > 0 {
		var timeoutCancel context.CancelFunc
		waitCtx, timeoutCancel = context.WithTimeout(runCtx, s.startupTimeout)
		defer timeoutCancel()
	}

	results := make(chan error, len(s.strategies))
	for _, strategy := range s.strategies {
		go func() { results <- strategy.WaitUntilReady(waitCtx, target) }()
	}
	var errs []error
	for range s.strategies {
		select {
		case err := <-results:
			if callerErr := callerCtx.Err(); callerErr != nil {
				return errors.Join(append(errs, callerErr, err)...)
			}
			if waitErr := waitCtx.Err(); waitErr != nil {
				return errors.Join(append(errs, waitErr, err)...)
			}
			if err == nil {
				return nil
			}
			errs = append(errs, err)
		case <-waitCtx.Done():
			if callerErr := callerCtx.Err(); callerErr != nil {
				return errors.Join(append(errs, callerErr)...)
			}
			return errors.Join(append(errs, waitCtx.Err())...)
		}
	}
	return errors.Join(errs...)
}
