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
)

func TestReview91LeafPollRejectsStopDuringSuccessfulCheck(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}

	err := poll(
		context.Background(),
		options{startupTimeout: time.Second, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want stopped lifecycle failure", err)
	}
}

func TestReview91ForLogRejectsStopDuringMatch(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready\n")),
	}

	err := ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want stopped lifecycle failure", err)
	}
}

type review91TransientTailReader struct {
	sent bool
}

func (r *review91TransientTailReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "ready\n"), nil
	}
	return 0, errors.New("temporary log transport failure")
}

func (*review91TransientTailReader) Close() error { return nil }

func TestReview91TransientScannerErrorDoesNotCommitReplayOrOccurrence(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		&review91TransientTailReader{},
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\nready\n")),
	}

	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.logCalls; got != 3 {
		t.Fatalf("FollowLogs calls = %d, want transient partial replay to be discarded", got)
	}
}

type review91CancelingMissingTarget struct {
	cancel context.CancelFunc
}

func (*review91CancelingMissingTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*review91CancelingMissingTarget) Running(context.Context) (bool, error) { return true, nil }
func (*review91CancelingMissingTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*review91CancelingMissingTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *review91CancelingMissingTarget) State(context.Context) (State, error) {
	t.cancel()
	return StateUnknown, fmt.Errorf("inspect raced cancellation: %w", ErrTargetNotFound)
}

func TestReview91LeafStateProbePrefersCallerCancellation(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		target := &review91CancelingMissingTarget{cancel: cancel}
		err := poll(ctx, options{startupTimeout: time.Second, pollInterval: time.Millisecond}, target, "wait for test", func(context.Context) error { return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("error = %v, disappearance hid cancellation", err)
		}
	})

	t.Run("log", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		target := &review91CancelingMissingTarget{cancel: cancel}
		err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("error = %v, disappearance hid cancellation", err)
		}
	})
}

type review91DeadlineMissingTarget struct {
	stateCalls atomic.Int32
}

func (*review91DeadlineMissingTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*review91DeadlineMissingTarget) Running(context.Context) (bool, error) { return true, nil }
func (*review91DeadlineMissingTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*review91DeadlineMissingTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *review91DeadlineMissingTarget) State(ctx context.Context) (State, error) {
	t.stateCalls.Add(1)
	<-ctx.Done()
	return StateUnknown, fmt.Errorf("inspect raced deadline: %w", ErrTargetNotFound)
}

func TestReview91LeafStateProbePrefersDeadline(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		target := &review91DeadlineMissingTarget{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := poll(ctx, options{startupTimeout: time.Second, pollInterval: time.Millisecond}, target, "wait for test", func(context.Context) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("error = %v, disappearance hid deadline", err)
		}
	})

	t.Run("log", func(t *testing.T) {
		target := &review91DeadlineMissingTarget{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrTargetNotFound) {
			t.Fatalf("error = %v, disappearance hid deadline", err)
		}
	})
}
