package wait

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type exitResult struct {
	status ExitStatus
	err    error
}

// scriptedExitTarget implements ExitCodeTarget and replays a fixed sequence
// of observations before falling back to its last configured result.
type scriptedExitTarget struct {
	*fakeTarget
	mu       sync.Mutex
	results  []exitResult
	fallback exitResult
	calls    int
}

func newScriptedExitTarget(results ...exitResult) *scriptedExitTarget {
	target := &scriptedExitTarget{fakeTarget: newFakeTarget(), results: results}
	if len(results) > 0 {
		target.fallback = results[len(results)-1]
	}
	return target
}

func (t *scriptedExitTarget) ExitStatus(context.Context) (ExitStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if len(t.results) == 0 {
		return t.fallback.status, t.fallback.err
	}
	result := t.results[0]
	t.results = t.results[1:]
	return result.status, result.err
}

func (t *scriptedExitTarget) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func stoppedStatus(code int) ExitStatus {
	return ExitStatus{State: StateStopped, Reported: true, Code: code}
}

func TestForExitAcceptsZeroExitCode(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: stoppedStatus(0)})
	if err := ForExit().WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.callCount() != 1 {
		t.Errorf("calls = %d, want one observation", target.callCount())
	}
}

func TestForExitFailsImmediatelyOnUnexpectedExitCode(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: stoppedStatus(1)})
	start := time.Now()
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("WaitUntilReady succeeded for exit code 1")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want an immediate exit code failure", err)
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Errorf("error = %q, want the exit code diagnostic", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
	if calls := target.callCount(); calls != 1 {
		t.Errorf("calls = %d, want one observation", calls)
	}
}

func TestForExitWithExitCodeAcceptsConfiguredCode(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: stoppedStatus(42)})
	if err := ForExit().WithExitCode(42).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForExitWithExitCodeMatcherAcceptsResult(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: stoppedStatus(3)})
	if err := ForExit().
		WithExitCodeMatcher(func(code int) bool { return code == 3 }).
		WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForExitWaitsWhileContainerRuns(t *testing.T) {
	target := newScriptedExitTarget(
		exitResult{status: ExitStatus{State: StateRunning}},
		exitResult{status: ExitStatus{State: StateRunning}},
		exitResult{status: stoppedStatus(0)},
	)
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if calls := target.callCount(); calls < 3 {
		t.Errorf("calls = %d, want polling until the container stopped", calls)
	}
}

func TestForExitReportsMissingExitCode(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: ExitStatus{State: StateStopped}})
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("WaitUntilReady succeeded without an exit code")
	}
	if !strings.Contains(err.Error(), "did not report an exit code") {
		t.Errorf("error = %q, want the missing exit code diagnostic", err)
	}
	if calls := target.callCount(); calls != 1 {
		t.Errorf("calls = %d, want no retry after the container stopped", calls)
	}
}

func TestForExitFailsImmediatelyOnTerminalState(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: ExitStatus{State: StatePaused}})
	start := time.Now()
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("WaitUntilReady succeeded for a paused container")
	}
	if !strings.Contains(err.Error(), "container paused while waiting") {
		t.Errorf("error = %q, want the terminal state diagnostic", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForExitRetriesTransientError(t *testing.T) {
	transient := errors.New("temporary inspect failure")
	target := newScriptedExitTarget(
		exitResult{err: transient},
		exitResult{status: stoppedStatus(0)},
	)
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if calls := target.callCount(); calls != 2 {
		t.Errorf("calls = %d, want a retry after the transient error", calls)
	}
}

func TestForExitReturnsPermanentTargetError(t *testing.T) {
	target := newScriptedExitTarget(exitResult{err: ErrTargetNotFound})
	err := ForExit().
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound", err)
	}
	if calls := target.callCount(); calls != 1 {
		t.Errorf("calls = %d, want no retry after a permanent error", calls)
	}
}

func TestForExitStartupTimeout(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: ExitStatus{State: StateRunning}})
	err := ForExit().
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out after 40ms") {
		t.Errorf("error = %q, want the startup timeout diagnostic", err)
	}
}

func TestForExitPreservesCallerCancellation(t *testing.T) {
	target := newScriptedExitTarget(exitResult{status: ExitStatus{State: StateRunning}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ForExit().
		WithStartupTimeout(time.Second).
		WaitUntilReady(ctx, target)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestForExitRequiresExitCodeTarget(t *testing.T) {
	start := time.Now()
	err := ForExit().
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil {
		t.Fatal("WaitUntilReady succeeded without an ExitCodeTarget")
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("error = %v, want ErrInvalidConfiguration", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Strategy != "ForExit" || configErr.Field != "target" {
		t.Errorf("error = %v, want the target ConfigError", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForExitRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		strategy *ExitStrategy
		field    string
	}{
		{
			name:     "negative exit code",
			strategy: ForExit().WithExitCode(-1),
			field:    "exit code",
		},
		{
			name:     "nil matcher",
			strategy: ForExit().WithExitCodeMatcher(nil),
			field:    "exit code matcher",
		},
		{
			name:     "negative timeout",
			strategy: ForExit().WithStartupTimeout(-time.Second),
			field:    "startup timeout",
		},
		{
			name:     "negative poll interval",
			strategy: ForExit().WithPollInterval(-time.Millisecond),
			field:    "poll interval",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.strategy.WaitUntilReady(context.Background(), newScriptedExitTarget())
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Field != tc.field {
				t.Fatalf("error = %v, want field %q", err, tc.field)
			}
		})
	}
}

func TestForExitBuildersDoNotMutateReceiver(t *testing.T) {
	base := ForExit()
	derived := base.WithExitCode(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond)

	if base.exitCodeSet || base.exitCode != 0 || base.exitMatcher != nil ||
		base.startupTimeout != 0 || base.pollInterval != 0 {
		t.Errorf("base strategy was mutated: %+v", base)
	}
	if !derived.exitCodeSet || derived.exitCode != 2 ||
		derived.startupTimeout != time.Second || derived.pollInterval != time.Millisecond {
		t.Errorf("derived strategy = %+v", derived)
	}

	matcherDerived := base.WithExitCode(2).WithExitCodeMatcher(func(int) bool { return true })
	if matcherDerived.exitCodeSet || matcherDerived.exitMatcher == nil {
		t.Errorf("matcher did not clear the fixed exit code: %+v", matcherDerived)
	}
}
