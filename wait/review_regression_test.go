package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type reviewTypedCause struct {
	message string
}

func (e *reviewTypedCause) Error() string { return e.message }

type reviewNoopTarget struct{}

func (*reviewNoopTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (*reviewNoopTarget) Running(context.Context) (bool, error) { return true, nil }
func (*reviewNoopTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*reviewNoopTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

type reviewExecTarget struct {
	cause        error
	block        bool
	started      chan struct{}
	startedOnce  sync.Once
	runningCalls atomic.Int32
}

func newReviewExecTarget(cause error, block bool) *reviewExecTarget {
	return &reviewExecTarget{
		cause:   cause,
		block:   block,
		started: make(chan struct{}),
	}
}

func (*reviewExecTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *reviewExecTarget) Running(ctx context.Context) (bool, error) {
	t.runningCalls.Add(1)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-timer.C:
		return true, errors.New("diagnostic probe fallback")
	}
}
func (*reviewExecTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *reviewExecTarget) ExecCommand(ctx context.Context, _ []string) (int, error) {
	t.startedOnce.Do(func() { close(t.started) })
	if t.block {
		<-ctx.Done()
	}
	return 0, t.cause
}

func TestPollRetainsCheckErrorWhenContextCompletes(t *testing.T) {
	cause := &reviewTypedCause{message: "backend failed at deadline"}
	ctx, cancel := context.WithCancel(context.Background())
	err := poll(ctx, options{startupTimeout: time.Hour}, &reviewNoopTarget{}, "wait", func(context.Context) error {
		cancel()
		return cause
	}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want current check cause", err)
	}
	var typed *reviewTypedCause
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want typed check cause", err)
	}
	if got := errors.Unwrap(err); got != context.Canceled {
		t.Fatalf("errors.Unwrap = %v, want context.Canceled", got)
	}
}

func TestPollRetainsCheckErrorAtCallerDeadline(t *testing.T) {
	cause := &reviewTypedCause{message: "backend failed at deadline"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := poll(ctx, options{startupTimeout: time.Hour}, &reviewNoopTarget{}, "wait", func(checkCtx context.Context) error {
		<-checkCtx.Done()
		return cause
	}, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want current check cause", err)
	}
}

func TestPollSkipsDiagnosticProbeAfterCallerTermination(t *testing.T) {
	cause := &reviewTypedCause{message: "readiness backend failure"}
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		target := newReviewExecTarget(cause, true)
		done := make(chan error, 1)
		go func() {
			done <- ForExec([]string{"probe"}).WithStartupTimeout(time.Hour).WithPollInterval(5*time.Millisecond).WaitUntilReady(ctx, target)
		}()
		select {
		case <-target.started:
			cancel()
		case <-time.After(time.Second):
			t.Fatal("exec check did not start")
		}
		var err error
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("wait did not return after cancellation")
		}
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("error = %v, want cancellation and typed cause", err)
		}
		var typed *reviewTypedCause
		if !errors.As(err, &typed) {
			t.Fatalf("error = %v, want typed cause", err)
		}
		if got := errors.Unwrap(err); got != context.Canceled {
			t.Fatalf("errors.Unwrap = %v, want context.Canceled", got)
		}
		if target.runningCalls.Load() != 0 {
			t.Fatalf("Running calls = %d, want no probe after cancellation", target.runningCalls.Load())
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		target := newReviewExecTarget(cause, false)
		start := time.Now()
		err := ForExec([]string{"probe"}).WithStartupTimeout(time.Hour).WithPollInterval(5*time.Millisecond).WaitUntilReady(ctx, target)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
			t.Fatalf("error = %v, want deadline and typed cause", err)
		}
		var typed *reviewTypedCause
		if !errors.As(err, &typed) {
			t.Fatalf("error = %v, want typed cause", err)
		}
		if got := errors.Unwrap(err); got != context.DeadlineExceeded {
			t.Fatalf("errors.Unwrap = %v, want context.DeadlineExceeded", got)
		}
		if target.runningCalls.Load() != 0 {
			t.Fatalf("Running calls = %d, want no probe after deadline", target.runningCalls.Load())
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("wait took %v, diagnostic probe outlived caller deadline", elapsed)
		}
	})
}

func TestReceiveLogScanResultPrefersQueuedTerminalCause(t *testing.T) {
	cause := &reviewTypedCause{message: "scanner backend failure"}
	t.Run("channel-queued", func(t *testing.T) {
		results := make(chan logScanResult, 1)
		results <- logScanResult{err: cause}
		done := make(chan struct{})
		close(done)

		result, ok := receiveLogScanResult(results, &logScanState{}, done)
		if !ok || result.err != cause {
			t.Fatalf("result = %+v, ok = %v; want queued scanner cause", result, ok)
		}
	})
	t.Run("published-before-send", func(t *testing.T) {
		results := make(chan logScanResult)
		state := &logScanState{}
		state.publish(logScanResult{err: cause})
		done := make(chan struct{})
		close(done)

		result, ok := receiveLogScanResult(results, state, done)
		if !ok || result.err != cause {
			t.Fatalf("result = %+v, ok = %v; want published scanner cause", result, ok)
		}
	})
}

func TestHandleLogScanResultKeepsContextAndQueuedCause(t *testing.T) {
	cause := &reviewTypedCause{message: "scanner backend failure"}
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "caller-cancel",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "caller-deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callerCtx, cancel := tc.ctx()
			defer cancel()
			waitCtx, waitCancel := context.WithTimeout(callerCtx, time.Hour)
			defer waitCancel()
			err := handleLogScanResult(callerCtx, waitCtx, time.Hour, "wait for log", &reviewNoopTarget{}, logScanResult{err: cause})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want queued scanner cause", err)
			}
			var typed *reviewTypedCause
			if !errors.As(err, &typed) {
				t.Fatalf("error = %v, want typed scanner cause", err)
			}
		})
	}
}

type reviewManualDeadlineContext struct {
	deadline time.Time
	done     chan struct{}
}

func newReviewManualDeadlineContext(deadline time.Time) *reviewManualDeadlineContext {
	return &reviewManualDeadlineContext{deadline: deadline, done: make(chan struct{})}
}

func (c *reviewManualDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *reviewManualDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *reviewManualDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (*reviewManualDeadlineContext) Value(any) any { return nil }

func (c *reviewManualDeadlineContext) expire() { close(c.done) }

type reviewLogTerminationTarget struct {
	terminate    func()
	runningCalls atomic.Int32
}

func (t *reviewLogTerminationTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *reviewLogTerminationTarget) Running(context.Context) (bool, error) {
	t.runningCalls.Add(1)
	return true, nil
}
func (t *reviewLogTerminationTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.terminate()
	return io.NopCloser(strings.NewReader("")), nil
}
func (*reviewLogTerminationTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

func TestForLogDoesNotProbeAfterCallerTermination(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		target := &reviewLogTerminationTarget{terminate: cancel}
		err := ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, target)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if got := errors.Unwrap(err); got != context.Canceled {
			t.Fatalf("errors.Unwrap = %v, want context.Canceled", got)
		}
		if target.runningCalls.Load() != 0 {
			t.Fatalf("Running calls = %d, want no probe after cancellation", target.runningCalls.Load())
		}
	})

	t.Run("deadline", func(t *testing.T) {
		caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
		target := &reviewLogTerminationTarget{terminate: caller.expire}
		err := ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(caller, target)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if got := errors.Unwrap(err); got != context.DeadlineExceeded {
			t.Fatalf("errors.Unwrap = %v, want context.DeadlineExceeded", got)
		}
		if target.runningCalls.Load() != 0 {
			t.Fatalf("Running calls = %d, want no probe after deadline", target.runningCalls.Load())
		}
	})
}

type reviewAnyStrategy struct {
	cause   error
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func (s *reviewAnyStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.cause
}

func TestForAnyCollectsContextAwareChildCause(t *testing.T) {
	cause := &reviewTypedCause{message: "child backend failure"}
	cases := []struct {
		name string
		make func() (context.Context, context.CancelFunc)
		want error
	}{
		{
			name: "caller-cancel",
			make: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			want: context.Canceled,
		},
		{
			name: "caller-deadline",
			make: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 25*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "startup-timeout",
			make: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			want: context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.make()
			defer cancel()
			child := &reviewAnyStrategy{cause: cause, delay: 5 * time.Millisecond, started: make(chan struct{})}
			strategy := ForAny(child)
			if tc.name == "startup-timeout" {
				strategy = strategy.WithStartupTimeout(25 * time.Millisecond)
			} else {
				strategy = strategy.WithStartupTimeout(time.Hour)
			}
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- strategy.WaitUntilReady(ctx, &reviewNoopTarget{}) }()
			if tc.name == "caller-cancel" {
				select {
				case <-child.started:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("child did not start")
				}
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("ForAny did not collect child result")
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want %v and child cause", err, tc.want)
			}
			var typed *reviewTypedCause
			if !errors.As(err, &typed) {
				t.Fatalf("error = %v, want typed child cause", err)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("ForAny took %v, want bounded result collection", elapsed)
			}
		})
	}
}

type reviewReleaseStrategy struct {
	started    chan struct{}
	release    <-chan struct{}
	canceled   chan struct{}
	err        error
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (s *reviewReleaseStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	s.startOnce.Do(func() { close(s.started) })
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.canceled) })
	<-s.release
	return s.err
}

func TestForAllClassifiesDeadlineSourceAtRaceBoundary(t *testing.T) {
	cases := []struct {
		name             string
		startupTimeout   time.Duration
		callerDeadline   time.Duration
		wantStartupLabel bool
	}{
		{name: "internal-timeout-first", startupTimeout: 25 * time.Millisecond, callerDeadline: 500 * time.Millisecond, wantStartupLabel: true},
		{name: "caller-deadline-first", startupTimeout: 500 * time.Millisecond, callerDeadline: 25 * time.Millisecond, wantStartupLabel: false},
		{name: "equal-deadlines", startupTimeout: 25 * time.Millisecond, callerDeadline: 25 * time.Millisecond, wantStartupLabel: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			caller := newReviewManualDeadlineContext(time.Now().Add(tc.callerDeadline))
			started := make(chan struct{})
			release := make(chan struct{})
			canceled := make(chan struct{})
			childCause := &reviewTypedCause{message: "child failure"}
			strategy := ForAll(&reviewReleaseStrategy{started: started, release: release, canceled: canceled, err: childCause}).
				WithStartupTimeout(tc.startupTimeout)
			done := make(chan error, 1)
			go func() { done <- strategy.WaitUntilReady(caller, &reviewNoopTarget{}) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("child did not start")
			}
			if tc.wantStartupLabel {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("internal startup timeout did not reach child")
				}
				caller.expire()
			} else {
				caller.expire()
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("caller deadline did not reach child")
				}
			}
			close(release)
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("ForAll did not finish")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want context.DeadlineExceeded", err)
			}
			if !errors.Is(err, childCause) {
				t.Fatalf("error = %v, want child cause", err)
			}
			hasStartup := strings.Contains(err.Error(), "startup timeout")
			hasCaller := strings.Contains(err.Error(), "caller deadline")
			if hasStartup != tc.wantStartupLabel || hasCaller == tc.wantStartupLabel {
				t.Fatalf("error = %v, startup=%v caller=%v; want startup=%v", err, hasStartup, hasCaller, tc.wantStartupLabel)
			}
		})
	}
}
