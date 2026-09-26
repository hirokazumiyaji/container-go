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
	var cancel context.CancelFunc
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		waitCtx, cancel = context.WithTimeout(ctx, s.startupTimeout)
		defer cancel()
	}
	callerDeadline, callerHasDeadline := callerCtx.Deadline()
	for i, strategy := range s.strategies {
		if err := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("before strategy %d", i)); err != nil {
			return err
		}
		err := strategy.WaitUntilReady(waitCtx, target)
		if err != nil {
			if terminal := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("in strategy %d", i)); terminal != nil {
				return withWaitCause(terminal, err, fmt.Sprintf("%v (strategy %d: %v)", terminal, i, err))
			}
			return err
		}
		// Only gate the next strategy on the context. Once the last one has
		// succeeded the composite wait is satisfied, and a context that
		// ended during final bookkeeping must not turn it into a failure.
		if i < len(s.strategies)-1 {
			if err := allContextError(callerCtx, waitCtx, s.startupTimeout, startupDeadline, callerDeadline, callerHasDeadline, fmt.Sprintf("after strategy %d", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func allContextError(callerCtx, waitCtx context.Context, startupTimeout time.Duration, startupDeadline time.Time, callerDeadline time.Time, callerHasDeadline bool, phase string) error {
	if err := callerCtx.Err(); err != nil {
		// If the caller's deadline is later than the composite startup
		// deadline, the child can observe the composite deadline first;
		// the parent may only record Err afterward. Classify by the
		// earlier deadline rather than by the later observation.
		if errors.Is(err, context.DeadlineExceeded) {
			if startupTimeout > 0 && callerHasDeadline && startupDeadline.Before(callerDeadline) {
				return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed %s", startupTimeout, phase), err)
			}
			return newWaitError(fmt.Sprintf("wait for all: caller deadline elapsed %s: %v", phase, err), err)
		}
		return newWaitError(fmt.Sprintf("wait for all: caller context ended %s: %v", phase, err), err)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && startupTimeout > 0 {
			if callerHasDeadline && !callerDeadline.After(startupDeadline) {
				return newWaitError(fmt.Sprintf("wait for all: caller deadline elapsed %s", phase), err)
			}
			return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed %s", startupTimeout, phase), err)
		}
		return newWaitError(fmt.Sprintf("wait for all: context ended %s: %v", phase, err), err)
	}
	return nil
}

func withWaitCause(primary error, cause error, message string) error {
	causes := make([]error, 0, 2)
	if primary != nil {
		causes = append(causes, primary)
	}
	if cause != nil {
		causes = append(causes, cause)
	}
	return newWaitError(message, causes...)
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
	callerCtx := ctx
	if err := callerCtx.Err(); err != nil {
		return newWaitError(fmt.Sprintf("wait for any: caller context ended before waiting: %v", err), err)
	}
	if len(s.strategies) == 0 {
		return nil
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
		go func(strategy Strategy) {
			results <- strategy.WaitUntilReady(waitCtx, target)
		}(strategy)
	}

	var errs []error
	for remaining := len(s.strategies); remaining > 0; {
		select {
		case err := <-results:
			remaining--
			// A result already published wins over a timer that fired
			// afterwards, so a strategy that succeeded just before the
			// deadline is not reported as a timeout.
			if err == nil {
				return nil
			}
			if terminal := anyContextError(callerCtx, waitCtx, s.startupTimeout); terminal != nil {
				errs = append(errs, err)
				return anyTerminalError(terminal, errs)
			}
			errs = append(errs, err)
		case <-waitCtx.Done():
			terminal := anyContextError(callerCtx, waitCtx, s.startupTimeout)
			if terminal == nil {
				terminal = newWaitError("wait for any: context ended", context.Canceled)
			}
			// A custom Strategy is allowed to ignore cancellation. Give
			// cooperative children only a small, bounded collection window
			// for their final causes, then return without waiting for the
			// remaining children.
			drain := time.NewTimer(anyResultDrainWindow(s.startupTimeout))
			defer drain.Stop()
			for remaining > 0 {
				select {
				case err := <-results:
					remaining--
					// Same rule as above: an observed success wins over a
					// context that ended while it was being handed over.
					if err == nil {
						return nil
					}
					errs = append(errs, err)
				case <-drain.C:
					return anyTerminalError(terminal, errs)
				}
			}
			return anyTerminalError(terminal, errs)
		}
	}
	return errors.Join(errs...)
}

// anyResultDrainWindow gives cooperative children a short opportunity to
// publish their terminal cause without turning cancellation into a second
// unbounded wait for a custom strategy.
func anyResultDrainWindow(startupTimeout time.Duration) time.Duration {
	const maxDrain = 10 * time.Millisecond
	if startupTimeout > 0 && startupTimeout/2 < maxDrain {
		return startupTimeout / 2
	}
	return maxDrain
}

func anyTerminalError(terminal error, errs []error) error {
	primary := terminal
	if cause := errors.Unwrap(terminal); cause != nil {
		primary = cause
	}
	causes := make([]error, 0, len(errs)+2)
	causes = append(causes, primary, terminal)
	causes = append(causes, errs...)
	// The message must name the strategies that failed. waitError.Error
	// renders only this text, so the causes below would otherwise be
	// reachable through errors.Is alone and invisible in a log.
	message := terminal.Error()
	for _, err := range errs {
		if err != nil {
			message += "; " + err.Error()
		}
	}
	return newWaitError(message, causes...)
}

func anyContextError(callerCtx, waitCtx context.Context, startupTimeout time.Duration) error {
	if err := callerCtx.Err(); err != nil {
		return newWaitError(fmt.Sprintf("wait for any: caller context ended: %v", err), err)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && startupTimeout > 0 {
			return newWaitError(fmt.Sprintf("wait for any: startup timeout %v elapsed", startupTimeout), err)
		}
		return newWaitError(fmt.Sprintf("wait for any: context ended: %v", err), err)
	}
	return nil
}
