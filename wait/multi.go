package wait

import (
	"context"
	"errors"
)

type allStrategy struct {
	strategies []Strategy
}

// ForAll waits for every strategy, in order.
func ForAll(strategies ...Strategy) Strategy {
	return &allStrategy{strategies: strategies}
}

func (s *allStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	for _, strategy := range s.strategies {
		if err := strategy.WaitUntilReady(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

type anyStrategy struct {
	strategies []Strategy
}

// ForAny waits until one of the strategies succeeds.
func ForAny(strategies ...Strategy) Strategy {
	return &anyStrategy{strategies: strategies}
}

func (s *anyStrategy) WaitUntilReady(ctx context.Context, target Target) error {
	if len(s.strategies) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
