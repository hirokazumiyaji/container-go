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
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, startupDeadline)
		defer cancel()
	}
	callerDeadline, callerHasDeadline := callerCtx.Deadline()

	// A derived context only exposes whichever deadline wins. Compare the
	// caller's configured deadline with the independent startup deadline
	// so equal or near-equal timers cannot be mislabeled at the boundary.
	callerOwnsTermination := func(waitErr error) bool {
		if errors.Is(waitErr, context.Canceled) {
			return true
		}
		if s.startupTimeout <= 0 || !errors.Is(waitErr, context.DeadlineExceeded) {
			return false
		}
		return callerHasDeadline && !callerDeadline.After(startupDeadline)
	}
	terminationCause := func(waitErr error) error {
		if !callerOwnsTermination(waitErr) {
			return waitErr
		}
		if callerErr := callerCtx.Err(); callerErr != nil {
			return callerErr
		}
		if errors.Is(waitErr, context.Canceled) {
			return context.Canceled
		}
		return context.DeadlineExceeded
	}

	// wrap classifies waitCtx termination for one phase of the loop
	// ("before"/"in"/"after"). phaseTail is the substring following the
	// classification label ("before strategy %d ran", "in strategy %d",
	// "after strategy %d"). childErr, when set, is the strategy's own
	// error and joins both the message tail and the cause list; only
	// the "in" phase passes one.
	wrap := func(phaseTail string, i int, childErr error) error {
		waitErr := waitCtx.Err()
		var extra []error
		suffix := ""
		if childErr != nil {
			suffix = fmt.Sprintf(": %v", childErr)
			extra = []error{childErr}
		}
		switch {
		case callerOwnsTermination(waitErr):
			label := "caller deadline elapsed"
			if errors.Is(waitErr, context.Canceled) {
				label = "caller context ended"
			}
			return newWaitError(fmt.Sprintf("wait for all: %s %s%s", label, phaseTail, suffix),
				append([]error{terminationCause(waitErr)}, extra...)...)
		case s.startupTimeout > 0 && errors.Is(waitErr, context.DeadlineExceeded):
			return newWaitError(fmt.Sprintf("wait for all: startup timeout %v elapsed %s%s", s.startupTimeout, phaseTail, suffix),
				append([]error{waitErr}, extra...)...)
		case childErr != nil:
			return newWaitError(fmt.Sprintf("wait for all: strategy %d%s", i, suffix),
				append([]error{waitErr}, extra...)...)
		default:
			return newWaitError(fmt.Sprintf("wait for all: context ended %s", phaseTail), waitErr)
		}
	}

	for i, strategy := range s.strategies {
		if waitCtx.Err() != nil {
			return wrap(fmt.Sprintf("before strategy %d ran", i), i, nil)
		}
		if err := callerCtx.Err(); err != nil {
			return newWaitError(fmt.Sprintf("wait for all: caller context ended before strategy %d ran", i), err)
		}

		if err := strategy.WaitUntilReady(waitCtx, target); err != nil {
			if waitCtx.Err() != nil {
				return wrap(fmt.Sprintf("in strategy %d", i), i, err)
			}
			if callerErr := callerCtx.Err(); callerErr != nil {
				return newWaitError(fmt.Sprintf("wait for all: caller context ended in strategy %d: %v", i, err), callerErr, err)
			}
			return err
		}

		if waitCtx.Err() != nil {
			return wrap(fmt.Sprintf("after strategy %d", i), i, nil)
		}
		if err := callerCtx.Err(); err != nil {
			return newWaitError(fmt.Sprintf("wait for all: caller context ended after strategy %d", i), err)
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
	remaining := len(s.strategies)
	var terminalErr error
	terminationErr := func() error {
		if err := callerCtx.Err(); err != nil {
			return err
		}
		return waitCtx.Err()
	}
	for remaining > 0 {
		if terminalErr != nil {
			// A context-aware strategy must publish its final error
			// after cancellation. Drain every result so its last cause
			// is not lost to ForAny's earlier context select.
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
			if termErr := terminationErr(); termErr != nil {
				terminalErr = termErr
			}
			if err != nil {
				errs = append(errs, err)
			}
			if terminalErr == nil && err == nil {
				return nil
			}
		case <-waitCtx.Done():
			terminalErr = terminationErr()
		}
	}
	if terminalErr != nil {
		errs = append(errs, terminalErr)
	}
	return errors.Join(errs...)
}
