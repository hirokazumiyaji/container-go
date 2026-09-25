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
// strategies in the sequence. A zero d leaves the sequence unbounded,
// relying on each strategy's own startup timeout. A negative d is
// invalid.
func (s *AllStrategy) WithStartupTimeout(d time.Duration) *AllStrategy {
	s.startupTimeout = d
	return s
}

func (s *AllStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := validateDuration("startup timeout", s.startupTimeout); err != nil {
		return err
	}
	if err := validateStrategies(s.strategies); err != nil {
		return err
	}
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
// strategy to succeed. A zero d leaves the wait unbounded, relying on
// each strategy's own startup timeout. A negative d is invalid.
func (s *AnyStrategy) WithStartupTimeout(d time.Duration) *AnyStrategy {
	s.startupTimeout = d
	return s
}

func (s *AnyStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if err := validateDuration("startup timeout", s.startupTimeout); err != nil {
		return err
	}
	if err := validateStrategies(s.strategies); err != nil {
		return err
	}
	if len(s.strategies) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if s.startupTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, s.startupTimeout)
		defer timeoutCancel()
	}

	results := make(chan error, len(s.strategies))
	for _, strategy := range s.strategies {
		go func() { results <- strategy.WaitUntilReady(ctx, target) }()
	}
	var errs []error
	for range s.strategies {
		select {
		case err := <-results:
			if err == nil {
				return nil
			}
			errs = append(errs, err)
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	return errors.Join(errs...)
}
