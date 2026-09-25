package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type review90Cause struct{ message string }

func (e *review90Cause) Error() string { return e.message }

type review90LogProbeTarget struct {
	streamErr   error
	probeErr    error
	probeCalls  atomic.Int32
	probeAfter  atomic.Bool
	probeBudget time.Duration
}

func (*review90LogProbeTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("unused endpoint")
}

func (t *review90LogProbeTarget) Running(ctx context.Context) (bool, error) {
	t.probeCalls.Add(1)
	if deadline, ok := ctx.Deadline(); ok {
		t.probeBudget = time.Until(deadline)
	}
	<-ctx.Done()
	t.probeAfter.Store(true)
	return false, t.probeErr
}

func (t *review90LogProbeTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	if t.streamErr != nil {
		return review90ErrorReader{err: t.streamErr}, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (*review90LogProbeTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("unused exec")
}

type review90ErrorReader struct{ err error }

func (r review90ErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (review90ErrorReader) Close() error               { return nil }

func TestForLogEOFProbeHonorsCallerBudgetAndRetainsCauses(t *testing.T) {
	streamCause := &review90Cause{message: "stream read failed"}
	probeCause := &review90Cause{message: "state probe failed"}
	target := &review90LogProbeTarget{streamErr: streamCause, probeErr: probeCause}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, target)
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, streamCause) || !errors.Is(err, probeCause) {
		t.Fatalf("error = %v, want deadline and both transient causes", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("wait took %v, EOF probe exceeded caller budget", elapsed)
	}
	if target.probeCalls.Load() != 1 || !target.probeAfter.Load() {
		t.Fatalf("probe calls = %d, after = %v, want one bounded probe", target.probeCalls.Load(), target.probeAfter.Load())
	}
	if target.probeBudget <= 0 || target.probeBudget > 40*time.Millisecond {
		t.Fatalf("probe budget = %v, want caller deadline remainder", target.probeBudget)
	}
}

type review90ContextStrategy struct {
	cause  error
	starts chan struct{}
}

func (s *review90ContextStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	select {
	case <-s.starts:
	default:
		close(s.starts)
	}
	<-ctx.Done()
	return s.cause
}

func TestCompositeContextErrorsRetainChildCauses(t *testing.T) {
	cause := &review90Cause{message: "child failed at deadline"}
	tests := []struct {
		name     string
		strategy Strategy
	}{
		{
			name:     "all",
			strategy: ForAll((&review90ContextStrategy{cause: cause, starts: make(chan struct{})})).WithStartupTimeout(20 * time.Millisecond),
		},
		{
			name:     "any",
			strategy: ForAny((&review90ContextStrategy{cause: cause, starts: make(chan struct{})})).WithStartupTimeout(20 * time.Millisecond),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			err := tc.strategy.WaitUntilReady(context.Background(), &issue90Target{})
			if err == nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want deadline and child cause", err)
			}
			if time.Since(started) > 250*time.Millisecond {
				t.Fatalf("composite took %v, want bounded context handling", time.Since(started))
			}
		})
	}
}

func TestCompositeCallerCancellationWinsOverChildDeadline(t *testing.T) {
	cause := &review90Cause{message: "child saw cancellation"}
	child := &review90ContextStrategy{cause: cause, starts: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-child.starts
		cancel()
	}()
	err := ForAll(child).WithStartupTimeout(time.Hour).WaitUntilReady(ctx, &issue90Target{})
	if err == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want cancellation and child cause", err)
	}
}

type review90DeadlineContext struct {
	deadline time.Time
	done     chan struct{}
}

func newReview90DeadlineContext(deadline time.Time) *review90DeadlineContext {
	return &review90DeadlineContext{deadline: deadline, done: make(chan struct{})}
}
func (c *review90DeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *review90DeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *review90DeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (*review90DeadlineContext) Value(any) any { return nil }
func (c *review90DeadlineContext) expire()     { close(c.done) }

type review90ReleaseStrategy struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	cause    error
}

func (s *review90ReleaseStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return s.cause
}

func TestForAllDistinguishesCallerAndStartupDeadlines(t *testing.T) {
	cause := &review90Cause{message: "child released after deadline"}
	for _, tc := range []struct {
		name           string
		startupTimeout time.Duration
		callerDeadline time.Duration
		wantStartup    bool
	}{
		{name: "startup", startupTimeout: 20 * time.Millisecond, callerDeadline: time.Second, wantStartup: true},
		{name: "caller", startupTimeout: time.Second, callerDeadline: 20 * time.Millisecond, wantStartup: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := newReview90DeadlineContext(time.Now().Add(tc.callerDeadline))
			child := &review90ReleaseStrategy{
				started:  make(chan struct{}),
				canceled: make(chan struct{}),
				release:  make(chan struct{}),
				cause:    cause,
			}
			done := make(chan error, 1)
			go func() {
				done <- ForAll(child).WithStartupTimeout(tc.startupTimeout).WaitUntilReady(caller, &issue90Target{})
			}()
			select {
			case <-child.started:
			case <-time.After(time.Second):
				t.Fatal("child did not start")
			}
			if tc.wantStartup {
				select {
				case <-child.canceled:
				case <-time.After(time.Second):
					t.Fatal("startup timeout did not reach child")
				}
			} else {
				caller.expire()
				select {
				case <-child.canceled:
				case <-time.After(time.Second):
					t.Fatal("caller deadline did not reach child")
				}
			}
			close(child.release)
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("composite did not finish")
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want deadline and child cause", err)
			}
			hasStartup := strings.Contains(err.Error(), "startup timeout")
			hasCaller := strings.Contains(err.Error(), "caller deadline")
			if hasStartup != tc.wantStartup || hasCaller == tc.wantStartup {
				t.Fatalf("error = %v, startup=%v caller=%v; want startup=%v", err, hasStartup, hasCaller, tc.wantStartup)
			}
		})
	}
}
