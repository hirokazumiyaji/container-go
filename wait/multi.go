package wait

import (
	"context"
	"errors"
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
// strategies in the sequence.
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
	for _, strategy := range s.strategies {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := strategy.WaitUntilReady(ctx, target); err != nil {
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
// strategy to succeed.
func (s *AnyStrategy) WithStartupTimeout(d time.Duration) *AnyStrategy {
	s.startupTimeout = d
	return s
}

func (s *AnyStrategy) WaitUntilReady(ctx context.Context, target Target) error {
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
		err := <-results
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
