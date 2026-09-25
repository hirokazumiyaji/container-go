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
	if s.startupTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.startupTimeout)
		defer cancel()
	}
	for i, strategy := range s.strategies {
		if err := ctx.Err(); err != nil {
			if s.startupTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("wait for all: startup timeout %v elapsed before strategy %d ran: %w", s.startupTimeout, i, err)
			}
			return err
		}
		if err := strategy.WaitUntilReady(ctx, target); err != nil {
			if s.startupTimeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("wait for all: startup timeout %v elapsed in strategy %d: %w", s.startupTimeout, i, err)
			}
			return err
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
	if err := ctx.Err(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitCtx := runCtx
	if s.startupTimeout > 0 {
		var timeoutCancel context.CancelFunc
		waitCtx, timeoutCancel = context.WithTimeout(waitCtx, s.startupTimeout)
		defer timeoutCancel()
	}

	results := make(chan error, len(s.strategies))
	for _, strategy := range s.strategies {
		go func(strategy Strategy) {
			results <- strategy.WaitUntilReady(waitCtx, target)
		}(strategy)
	}

	var errs []error
	remaining := len(s.strategies)
	var terminalErr error
	for remaining > 0 {
		if terminalErr != nil {
			// Cancellation is cooperative. Drain every strategy before
			// returning so a canceled ForExec/stream has completed its
			// local Wait/reap path and its ExecTerminationError is not left
			// in an abandoned goroutine.
			err := <-results
			remaining--
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}

		select {
		case err := <-results:
			remaining--
			if err != nil {
				errs = append(errs, err)
			}
			if waitCtx.Err() != nil {
				terminalErr = waitCtx.Err()
				continue
			}
			if err == nil {
				// ForAny's success contract is first-success-wins. Cancel
				// the other strategies, but wait for their terminal errors
				// before returning so their lifecycle work is complete.
				cancel()
				for remaining > 0 {
					loserErr := <-results
					remaining--
					_ = loserErr // success remains the public result
				}
				return nil
			}
		case <-waitCtx.Done():
			terminalErr = waitCtx.Err()
			if terminalErr == nil {
				terminalErr = context.Canceled
			}
		}
	}
	if terminalErr != nil {
		errs = append(errs, terminalErr)
	}
	return errors.Join(errs...)
}
