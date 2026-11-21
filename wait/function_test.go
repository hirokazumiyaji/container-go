package wait

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForFuncRetriesUntilReady(t *testing.T) {
	var calls atomic.Int32
	err := ForFunc(func(context.Context, Target) error {
		if calls.Add(1) < 3 {
			return errors.New("not ready yet")
		}
		return nil
	}).WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("callback calls = %d, want at least 3", got)
	}
}

func TestForFuncTimeoutRetainsCallbackError(t *testing.T) {
	cause := errors.New("cache is still loading")
	err := ForFunc(func(context.Context, Target) error {
		return cause
	}).WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error = %v, want the callback cause", err)
	}
	if !strings.Contains(err.Error(), "wait for custom function") {
		t.Errorf("error = %q, want the strategy operation", err)
	}
}

func TestForFuncContextCancellationStopsImmediately(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	err := ForFunc(func(context.Context, Target) error {
		calls.Add(1)
		return fmt.Errorf("probe: %w", context.Canceled)
	}).WithStartupTimeout(5*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate termination", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("callback calls = %d, want 1", got)
	}
	// The wait context was not canceled, so the termination must not be
	// reported as caller cancellation.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, must not match a context sentinel", err)
	}
	if !strings.Contains(err.Error(), "custom wait function returned a context cancellation") {
		t.Errorf("error = %q, want the function termination wording", err)
	}
}

func TestForFuncRetriesPerAttemptDeadline(t *testing.T) {
	var calls atomic.Int32
	err := ForFunc(func(context.Context, Target) error {
		if calls.Add(1) < 3 {
			return fmt.Errorf("probe: %w", context.DeadlineExceeded)
		}
		return nil
	}).WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("callback calls = %d, want per-attempt deadlines retried", got)
	}
}

func TestForFuncContextCancellationKeepsBackendCause(t *testing.T) {
	backend := errors.New("backend probe failed")
	err := ForFunc(func(context.Context, Target) error {
		return errors.Join(context.Canceled, backend)
	}).WithStartupTimeout(5*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, backend) {
		t.Errorf("error = %v, want the backend cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, must not match context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "backend probe failed") {
		t.Errorf("error = %q, want the backend cause text", err)
	}
}

func TestForFuncPermanentErrorStopsImmediately(t *testing.T) {
	start := time.Now()
	err := ForFunc(func(context.Context, Target) error {
		return fmt.Errorf("invalid credentials: %w", ErrInvalidConfiguration)
	}).WithStartupTimeout(5*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("error = %v, want ErrInvalidConfiguration", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForFuncExitCheckFailsFastOnStoppedTarget(t *testing.T) {
	target := newFakeTarget()
	target.state.Store(StateStopped)
	var calls atomic.Int32
	err := ForFunc(func(context.Context, Target) error {
		calls.Add(1)
		return nil
	}).WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "container stopped while waiting") {
		t.Errorf("error = %q, want the stopped lifecycle failure", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("callback calls = %d, want none before the terminal state", got)
	}
}

func TestForFuncWithoutExitCheckKeepsPolling(t *testing.T) {
	target := newFakeTarget()
	target.state.Store(StateStopped)
	var calls atomic.Int32
	var observed atomic.Value
	err := ForFunc(func(ctx context.Context, waitTarget Target) error {
		calls.Add(1)
		if stateTarget, ok := waitTarget.(StateTarget); ok {
			if state, err := stateTarget.State(ctx); err == nil {
				observed.Store(state)
			}
		}
		return errors.New("still waiting")
	}).WithExitCheck(false).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("callback calls = %d, want repeated polling", got)
	}
	state, ok := observed.Load().(State)
	if !ok || state != StateStopped {
		t.Errorf("callback observed state = %v, want %q", observed.Load(), StateStopped)
	}
}

func TestForFuncSettersAndValidation(t *testing.T) {
	strategy := ForFunc(func(context.Context, Target) error { return nil }).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WithExitCheck(false)
	if strategy.startupTimeout != time.Second || strategy.pollInterval != time.Millisecond || strategy.exitCheck {
		t.Errorf("options = %+v", strategy.options)
	}
	if err := Validate(strategy); err != nil {
		t.Errorf("Validate: %v", err)
	}

	err := ForFunc(nil).WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("nil function unexpectedly accepted")
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Strategy != "ForFunc" {
		t.Fatalf("error = %v, want a ForFunc *ConfigError", err)
	}
}

func TestForFuncWithinComposite(t *testing.T) {
	var calls atomic.Int32
	err := ForAll(ForFunc(func(context.Context, Target) error {
		calls.Add(1)
		return nil
	})).WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if calls.Load() == 0 {
		t.Error("composite never ran the custom function")
	}
}
