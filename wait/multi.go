package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const compositeDrainTimeout = 100 * time.Millisecond

// AllStrategy waits for every strategy, in order. Before accepting each
// successful strategy it performs a bounded, fail-closed lifecycle check.
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
	if err := callerCtx.Err(); err != nil {
		return fmt.Errorf("wait for all: caller context ended before waiting: %w", err)
	}
	if len(s.strategies) == 0 {
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	waitCtx := runCtx
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		var timeoutCancel context.CancelFunc
		waitCtx, timeoutCancel = context.WithDeadline(runCtx, startupDeadline)
		defer timeoutCancel()
	}

	lifecycleErr, err := startLifecycleMonitor(waitCtx, target, "wait for all")
	if err != nil {
		return err
	}

	for i, strategy := range s.strategies {
		if err := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("before strategy %d", i)); err != nil {
			return err
		}

		result := make(chan error, 1)
		go func(strategy Strategy) {
			result <- strategy.WaitUntilReady(waitCtx, target)
		}(strategy)

		select {
		case lifecycleErrValue := <-lifecycleErr:
			runCancel()
			return lifecycleErrValue
		case err := <-result:
			// Prefer a lifecycle failure that was queued while the child
			// was finishing. This closes the terminal-transition race at
			// the boundary between custom and built-in strategies.
			select {
			case lifecycleErrValue := <-lifecycleErr:
				runCancel()
				return lifecycleErrValue
			default:
			}
			if err != nil {
				if contextErr := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("in strategy %d", i)); contextErr != nil {
					return errors.Join(contextErr, err)
				}
				return err
			}
			if contextErr := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("after strategy %d", i)); contextErr != nil {
				return contextErr
			}
			if finalErr := finalLifecycleCheck(waitCtx, target, "wait for all"); finalErr != nil {
				if contextErr := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("after strategy %d", i)); contextErr != nil {
					return errors.Join(contextErr, finalErr)
				}
				return finalErr
			}
			if contextErr := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("after strategy %d final lifecycle check", i)); contextErr != nil {
				return contextErr
			}
		case <-waitCtx.Done():
			contextErr := compositeContextError("wait for all", callerCtx, waitCtx, s.startupTimeout, startupDeadline, fmt.Sprintf("in strategy %d", i))
			if contextErr == nil {
				contextErr = context.Canceled
			}
			return drainCompositeResult(result, contextErr)
		}
	}
	return nil
}

// AnyStrategy waits until one of the strategies succeeds. Before accepting
// a success it performs a bounded, fail-closed lifecycle check.
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
		return fmt.Errorf("wait for any: caller context ended before waiting: %w", err)
	}
	if len(s.strategies) == 0 {
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	waitCtx := runCtx
	var startupDeadline time.Time
	if s.startupTimeout > 0 {
		startupDeadline = time.Now().Add(s.startupTimeout)
		var timeoutCancel context.CancelFunc
		waitCtx, timeoutCancel = context.WithDeadline(runCtx, startupDeadline)
		defer timeoutCancel()
	}

	lifecycleErr, err := startLifecycleMonitor(waitCtx, target, "wait for any")
	if err != nil {
		return err
	}
	if contextErr := compositeContextError("wait for any", callerCtx, waitCtx, s.startupTimeout, startupDeadline, "before strategies"); contextErr != nil {
		return contextErr
	}

	results := make(chan error, len(s.strategies))
	for _, strategy := range s.strategies {
		go func(strategy Strategy) {
			results <- strategy.WaitUntilReady(waitCtx, target)
		}(strategy)
	}

	var errs []error
	for range s.strategies {
		select {
		case lifecycleErrValue := <-lifecycleErr:
			runCancel()
			return lifecycleErrValue
		case err := <-results:
			select {
			case lifecycleErrValue := <-lifecycleErr:
				runCancel()
				return lifecycleErrValue
			default:
			}
			if contextErr := compositeContextError("wait for any", callerCtx, waitCtx, s.startupTimeout, startupDeadline, "after a strategy result"); contextErr != nil {
				return errors.Join(contextErr, err)
			}
			if err == nil {
				if finalErr := finalLifecycleCheck(waitCtx, target, "wait for any"); finalErr != nil {
					if contextErr := compositeContextError("wait for any", callerCtx, waitCtx, s.startupTimeout, startupDeadline, "after a successful strategy"); contextErr != nil {
						return errors.Join(contextErr, finalErr)
					}
					return finalErr
				}
				if contextErr := compositeContextError("wait for any", callerCtx, waitCtx, s.startupTimeout, startupDeadline, "after the final lifecycle check"); contextErr != nil {
					return contextErr
				}
				return nil
			}
			errs = append(errs, err)
		case <-waitCtx.Done():
			contextErr := compositeContextError("wait for any", callerCtx, waitCtx, s.startupTimeout, startupDeadline, "while waiting")
			if contextErr == nil {
				contextErr = context.Canceled
			}
			pending := len(s.strategies) - len(errs)
			if pending < 0 {
				pending = 0
			}
			childErrs := drainCompositeResults(results, pending)
			return errors.Join(append([]error{contextErr}, append(errs, childErrs...)...)...)
		}
	}
	return errors.Join(errs...)
}

// finalLifecycleCheck closes the ticker race at a successful strategy
// boundary. A composite never accepts success without one bounded state
// observation made after the child returns; probe failure is fail-closed.
func finalLifecycleCheck(ctx context.Context, target Target, what string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, stateCheckInterval)
	defer cancel()
	if err := probeCtx.Err(); err != nil {
		return err
	}
	state, err := targetState(probeCtx, target)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("%s: final lifecycle check: %w", what, err)
	}
	if terminalWaitState(state) {
		return stateFailure(what, state, nil, nil)
	}
	return nil
}

// startLifecycleMonitor performs the initial lifecycle check and then
// watches for terminal transitions while custom strategies run. The
// returned channel is buffered and the monitor stops when the caller's
// context ends; a custom strategy that ignores cancellation cannot hold up
// the composite strategy's fail-fast return.
func startLifecycleMonitor(ctx context.Context, target Target, what string) (<-chan error, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var lastStateErr error
	state, err := targetState(ctx, target)
	if err != nil {
		if permanentProbeError(err) {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		lastStateErr = err
	} else if terminalWaitState(state) {
		return nil, stateFailure(what, state, nil, lastStateErr)
	}

	lifecycleErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(stateCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state, err := targetState(ctx, target)
				if err != nil {
					if permanentProbeError(err) {
						lifecycleErr <- fmt.Errorf("%s: %w", what, err)
						return
					}
					lastStateErr = err
					continue
				}
				if terminalWaitState(state) {
					lifecycleErr <- stateFailure(what, state, nil, lastStateErr)
					return
				}
			}
		}
	}()
	return lifecycleErr, nil
}

func drainCompositeResult(result <-chan error, contextErr error) error {
	timer := time.NewTimer(compositeDrainTimeout)
	defer timer.Stop()
	select {
	case childErr := <-result:
		return errors.Join(contextErr, childErr)
	case <-timer.C:
		return contextErr
	}
}

func drainCompositeResults(results <-chan error, count int) []error {
	if count <= 0 {
		return nil
	}
	childErrs := make([]error, 0, count)
	received := 0
	timer := time.NewTimer(compositeDrainTimeout)
	defer timer.Stop()
	for received < count {
		select {
		case childErr := <-results:
			received++
			if childErr != nil {
				childErrs = append(childErrs, childErr)
			}
		case <-timer.C:
			return childErrs
		}
	}
	return childErrs
}

func compositeContextError(prefix string, callerCtx, waitCtx context.Context, startupTimeout time.Duration, startupDeadline time.Time, phase string) error {
	if err := callerCtx.Err(); err != nil {
		return fmt.Errorf("%s: caller context ended %s: %w", prefix, phase, err)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callerDeadlineWins(callerCtx, startupDeadline) {
			return fmt.Errorf("%s: caller deadline elapsed %s: %w", prefix, phase, context.DeadlineExceeded)
		}
		if startupTimeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s: startup timeout %v elapsed %s: %w", prefix, startupTimeout, phase, context.DeadlineExceeded)
		}
		return fmt.Errorf("%s: context ended %s: %w", prefix, phase, err)
	}
	return nil
}
