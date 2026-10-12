package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type review91CountingSuccessStrategy struct {
	calls atomic.Int32
}

func (s *review91CountingSuccessStrategy) WaitUntilReady(context.Context, Target) error {
	s.calls.Add(1)
	return nil
}

func TestReview91ForAllCancellationPrecedesLifecycleProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := newLifecycleTarget(StateRunning)
	strategy := &review91CountingSuccessStrategy{}

	err := ForAll(strategy).WaitUntilReady(ctx, target)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := target.stateCalls; got != 0 {
		t.Fatalf("State calls = %d, want no lifecycle probe", got)
	}
	if got := strategy.calls.Load(); got != 0 {
		t.Fatalf("strategy calls = %d, want no strategy start", got)
	}
}

func TestReview91CompositeCancellationPrecedesEmptyFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]Strategy{
		"all": ForAll(),
		"any": ForAny(),
	}
	for name, strategy := range cases {
		t.Run(name, func(t *testing.T) {
			if err := strategy.WaitUntilReady(ctx, newLifecycleTarget(StateRunning)); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestReview91LogReplayKeepsCursorAcrossEmptyAndPartialReconnects(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("boot\nready\n")),
		io.NopCloser(strings.NewReader("")),
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("boot\nready\nready\n")),
	}

	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.logCalls; got != 4 {
		t.Fatalf("FollowLogs calls = %d, want empty and partial reconnects to preserve the cursor", got)
	}
}

type review91PermanentStreamSetupTarget struct {
	calls atomic.Int32
}

func (*review91PermanentStreamSetupTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*review91PermanentStreamSetupTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *review91PermanentStreamSetupTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.calls.Add(1)
	return nil, fmt.Errorf("logs: %w: invalid runner", cli.ErrStreamSetup)
}
func (*review91PermanentStreamSetupTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

func TestReview91ForLogDoesNotRetryPermanentStreamSetup(t *testing.T) {
	target := &review91PermanentStreamSetupTarget{}
	err := ForLog("ready").
		WithStartupTimeout(300*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if !errors.Is(err, ErrLogStreamSetup) {
		t.Fatalf("error = %v, want ErrLogStreamSetup", err)
	}
	if got := target.calls.Load(); got != 1 {
		t.Fatalf("FollowLogs calls = %d, want fail-fast after setup error", got)
	}
}

type review91StopThenSuccessStrategy struct {
	target *lifecycleTarget
}

func (s review91StopThenSuccessStrategy) WaitUntilReady(context.Context, Target) error {
	s.target.mu.Lock()
	s.target.state = StateStopped
	s.target.mu.Unlock()
	return nil
}

func TestReview91CompositeRejectsSuccessAfterLifecycleStop(t *testing.T) {
	cases := map[string]func(Strategy) Strategy{
		"all": func(strategy Strategy) Strategy { return ForAll(strategy) },
		"any": func(strategy Strategy) Strategy { return ForAny(strategy) },
	}
	for name, wrap := range cases {
		t.Run(name, func(t *testing.T) {
			target := newLifecycleTarget(StateRunning)
			err := wrap(review91StopThenSuccessStrategy{target: target}).
				WaitUntilReady(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), "stopped") {
				t.Fatalf("error = %v, want stopped lifecycle failure", err)
			}
		})
	}
}

type review91SlowFinalStateTarget struct {
	calls          atomic.Int32
	firstStateDone chan struct{}
}

func (*review91SlowFinalStateTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*review91SlowFinalStateTarget) Running(context.Context) (bool, error) { return true, nil }
func (*review91SlowFinalStateTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*review91SlowFinalStateTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *review91SlowFinalStateTarget) State(ctx context.Context) (State, error) {
	if t.calls.Add(1) == 1 {
		if t.firstStateDone != nil {
			close(t.firstStateDone)
		}
		return StateRunning, nil
	}
	<-ctx.Done()
	return StateUnknown, ctx.Err()
}

type review91WaitForFirstStateStrategy struct {
	done <-chan struct{}
}

func (s review91WaitForFirstStateStrategy) WaitUntilReady(context.Context, Target) error {
	<-s.done
	return nil
}

func TestReview91CompositeFinalLifecycleCheckIsBounded(t *testing.T) {
	target := &review91SlowFinalStateTarget{firstStateDone: make(chan struct{})}
	start := time.Now()
	err := ForAll(review91WaitForFirstStateStrategy{done: target.firstStateDone}).WithStartupTimeout(100*time.Millisecond).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want bounded final lifecycle deadline", err)
	}
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("final lifecycle check took %v, want bounded under 1.5s", elapsed)
	}
	if got := target.calls.Load(); got < 2 {
		t.Fatalf("State calls = %d, want initial and final checks", got)
	}
}
