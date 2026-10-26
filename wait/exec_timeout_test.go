package wait

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingExecTarget models a readiness command that hangs until its attempt
// context is canceled.
type blockingExecTarget struct {
	*fakeTarget
	started   chan struct{}
	startOnce sync.Once
	errOnDone error
}

func (t *blockingExecTarget) ExecCommand(ctx context.Context, _ []string) (int, error) {
	call := t.execCalls.Add(1)
	t.startOnce.Do(func() {
		if t.started != nil {
			close(t.started)
		}
	})
	<-ctx.Done()
	if call == 1 && t.errOnDone != nil {
		return 1, t.errOnDone
	}
	return 1, ctx.Err()
}

type deadlineRecordingTarget struct {
	*fakeTarget
	hasDeadline bool
	deadline    time.Time
}

func (t *deadlineRecordingTarget) ExecCommand(ctx context.Context, _ []string) (int, error) {
	t.execCalls.Add(1)
	t.deadline, t.hasDeadline = ctx.Deadline()
	return 0, nil
}

func TestForExecGivesEachAttemptItsOwnTimeout(t *testing.T) {
	target := &deadlineRecordingTarget{fakeTarget: newFakeTarget()}

	err := ForExec([]string{"probe"}).
		WithExecTimeout(200*time.Millisecond).
		WithStartupTimeout(5*time.Second).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if n := target.execCalls.Load(); n != 1 {
		t.Fatalf("exec calls = %d, want one successful attempt", n)
	}
	if !target.hasDeadline {
		t.Fatal("ExecCommand context has no deadline")
	}
	if remaining := time.Until(target.deadline); remaining <= 0 || remaining > 200*time.Millisecond {
		t.Fatalf("attempt deadline is %v away, want at most 200ms", remaining)
	}
}

func TestForExecRetriesHungAttemptsWithinStartupTimeout(t *testing.T) {
	target := &blockingExecTarget{fakeTarget: newFakeTarget()}
	const startupTimeout = 300 * time.Millisecond

	start := time.Now()
	err := ForExec([]string{"probe"}).
		WithExecTimeout(20*time.Millisecond).
		WithStartupTimeout(startupTimeout).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want startup timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("error = %q, want final startup timeout text", err)
	}
	if !strings.Contains(err.Error(), "exec attempt timed out after 20ms") {
		t.Fatalf("error = %q, want the retried attempt timeout in diagnostics", err)
	}
	if errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, per-attempt timeout must not be a configuration error", err)
	}
	if n := target.execCalls.Load(); n < 2 {
		t.Fatalf("exec calls = %d, want retries after a hung attempt", n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitUntilReady took %v, want it bounded by the startup timeout", elapsed)
	}
}

func TestForExecDefaultAttemptTimeoutRetriesHungCommand(t *testing.T) {
	target := &blockingExecTarget{fakeTarget: newFakeTarget()}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(1500*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want startup timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "timed out after 1.5s") {
		t.Fatalf("error = %q, want final startup timeout text", err)
	}
	if !strings.Contains(err.Error(), "exec attempt timed out after 1s") {
		t.Fatalf("error = %q, want the derived attempt timeout in diagnostics", err)
	}
	if n := target.execCalls.Load(); n < 2 {
		t.Fatalf("exec calls = %d, want retries with the derived attempt timeout", n)
	}
}

func TestForExecAttemptTimeoutIsRetryableDespiteLaunchShapedError(t *testing.T) {
	launchErr := &exec.Error{Name: "probe", Err: errors.New("executable file not found in $PATH")}
	target := &blockingExecTarget{fakeTarget: newFakeTarget(), errOnDone: launchErr}

	err := ForExec([]string{"probe"}).
		WithExecTimeout(15*time.Millisecond).
		WithStartupTimeout(200*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want startup timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, per-attempt timeout must not be a configuration error", err)
	}
	var got *exec.Error
	if errors.As(err, &got) {
		t.Fatalf("error = %v, launch-shaped command error leaked into the terminal cause", err)
	}
	if n := target.execCalls.Load(); n < 2 {
		t.Fatalf("exec calls = %d, want retries instead of a terminal launch failure", n)
	}
}

func TestForExecAttemptTimeoutPreservesCallerCancellation(t *testing.T) {
	target := &blockingExecTarget{fakeTarget: newFakeTarget(), started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ForExec([]string{"probe"}).
			WithExecTimeout(time.Minute).
			WithStartupTimeout(time.Minute).
			WithPollInterval(5*time.Millisecond).
			WaitUntilReady(ctx, target)
	}()

	select {
	case <-target.started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("exec attempt did not start")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want caller cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, must not report the startup or attempt timeout", err)
		}
		if errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v, must not be a configuration error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return after caller cancellation")
	}
}

func TestExecAttemptTimeoutDefault(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		explicit time.Duration
		want     time.Duration
	}{
		{name: "default interval", interval: 0, want: 2500 * time.Millisecond},
		{name: "small interval floor", interval: 10 * time.Millisecond, want: time.Second},
		{name: "proportional", interval: 300 * time.Millisecond, want: 3 * time.Second},
		{name: "large interval cap", interval: time.Second, want: 5 * time.Second},
		{name: "explicit override", interval: 10 * time.Millisecond, explicit: 42 * time.Millisecond, want: 42 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strategy := ForExec([]string{"probe"})
			if tt.interval != 0 {
				strategy.WithPollInterval(tt.interval)
			}
			if tt.explicit != 0 {
				strategy.WithExecTimeout(tt.explicit)
			}
			if got := strategy.attemptTimeout(); got != tt.want {
				t.Fatalf("attemptTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestForExecRejectsNegativeExecTimeoutBeforeProbing(t *testing.T) {
	target := newFakeTarget()

	err := ForExec([]string{"probe"}).
		WithExecTimeout(-time.Millisecond).
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want configuration error")
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %T, want *ConfigError", err)
	}
	if configErr.Strategy != "ForExec" {
		t.Fatalf("Strategy = %q, want ForExec", configErr.Strategy)
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
	}
	if n := target.execCalls.Load(); n != 0 {
		t.Fatalf("exec calls = %d, want no probe before validation", n)
	}
}
