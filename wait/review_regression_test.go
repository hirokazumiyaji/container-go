package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	cause       error
	block       bool
	started     chan struct{}
	startedOnce sync.Once
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
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, nil
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
	})
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
	caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
	started := make(chan struct{})
	var startOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- poll(caller, options{startupTimeout: time.Hour}, &reviewNoopTarget{}, "wait", func(checkCtx context.Context) error {
			startOnce.Do(func() { close(started) })
			<-checkCtx.Done()
			return cause
		})
	}()
	select {
	case <-started:
		caller.expire()
	case <-time.After(time.Second):
		t.Fatal("poll check did not start")
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not return after deadline")
	}
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
	})

	t.Run("deadline", func(t *testing.T) {
		caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
		target := newReviewExecTarget(cause, false)
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- ForExec([]string{"probe"}).WithStartupTimeout(time.Hour).WithPollInterval(5*time.Millisecond).WaitUntilReady(caller, target)
		}()
		select {
		case <-target.started:
			caller.expire()
		case <-time.After(time.Second):
			t.Fatal("exec check did not start")
		}
		var err error
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("wait did not return after deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
			t.Fatalf("error = %v, want deadline and typed cause", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("wait took %v, diagnostic probe outlived caller deadline", elapsed)
		}
	})
}

func TestReceiveLogScanResultPrefersQueuedTerminalCause(t *testing.T) {
	cause := &reviewTypedCause{message: "scanner backend failure"}
	results := make(chan logScanResult, 1)
	results <- logScanResult{err: cause}
	done := make(chan struct{})
	close(done)
	matched := &atomic.Bool{}

	result, ok := receiveLogScanResult(context.Background(), nil, results, done, matched)
	if !ok || result.err != cause {
		t.Fatalf("result = %+v, ok = %v; want queued scanner cause", result, ok)
	}
}

type reviewLogReadStartTarget struct {
	started chan struct{}
}

func (t *reviewLogReadStartTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *reviewLogReadStartTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *reviewLogReadStartTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	// Signal from the reader's first Read (not FollowLogs) so cancellation
	// cannot precede the scan goroutine observing the stream.
	return &reviewLogReadStartCloser{ctx: ctx, started: t.started}, nil
}
func (*reviewLogReadStartTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

type reviewLogReadStartCloser struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (r *reviewLogReadStartCloser) Read([]byte) (int, error) {
	r.once.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (*reviewLogReadStartCloser) Close() error { return nil }

func TestForLogCancelSynchronizesOnFirstRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, &reviewLogReadStartTarget{started: started})
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("log Read did not start")
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("ForLog did not return after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ForLog took %v after cancellation", elapsed)
	}
}

type reviewLogDeadlineCauseTarget struct {
	cause   error
	started chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (t *reviewLogDeadlineCauseTarget) Endpoint(context.Context, string) (string, error) {
	return "", t.cause
}
func (t *reviewLogDeadlineCauseTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *reviewLogDeadlineCauseTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	// First open publishes the typed stream cause. Later opens signal that
	// retention has happened so the test can expire the caller deterministically.
	if t.calls.Add(1) == 1 {
		return &waitContractErrorReadCloser{err: t.cause}, nil
	}
	t.once.Do(func() {
		if t.started != nil {
			close(t.started)
		}
	})
	<-ctx.Done()
	return nil, ctx.Err()
}
func (t *reviewLogDeadlineCauseTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, t.cause
}

func TestForLogDeadlineRetainsStreamCause(t *testing.T) {
	cause := &reviewTypedCause{message: "scanner backend failure"}
	caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
	started := make(chan struct{})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).
			WaitUntilReady(caller, &reviewLogDeadlineCauseTarget{cause: cause, started: started})
	}()
	select {
	case <-started:
		caller.expire()
	case <-time.After(time.Second):
		t.Fatal("log stream cause was not retained before deadline")
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("ForLog did not return after deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want stream cause", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ForLog took %v, want bounded context handling", elapsed)
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
	terminate func()
	started   chan struct{}
	once      sync.Once
}

func (t *reviewLogTerminationTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *reviewLogTerminationTarget) Running(context.Context) (bool, error) {
	return true, nil
}
func (t *reviewLogTerminationTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.once.Do(func() {
		if t.started != nil {
			close(t.started)
		}
		t.terminate()
	})
	return io.NopCloser(strings.NewReader("")), nil
}
func (*reviewLogTerminationTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

func TestForLogDoesNotOutliveCallerTermination(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		target := &reviewLogTerminationTarget{terminate: cancel, started: started}
		start := time.Now()
		err := ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, target)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("ForLog took %v after cancellation", elapsed)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
		target := &reviewLogTerminationTarget{terminate: caller.expire, started: make(chan struct{})}
		start := time.Now()
		err := ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(caller, target)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("ForLog took %v after deadline", elapsed)
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
		name    string
		want    error
		startup time.Duration
		trigger func(t *testing.T, child *reviewAnyStrategy) (context.Context, context.CancelFunc)
	}{
		{
			name:    "caller-cancel",
			want:    context.Canceled,
			startup: time.Hour,
			trigger: func(t *testing.T, child *reviewAnyStrategy) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					select {
					case <-child.started:
						cancel()
					case <-time.After(time.Second):
						t.Error("child did not start")
						cancel()
					}
				}()
				return ctx, cancel
			},
		},
		{
			name:    "caller-deadline",
			want:    context.DeadlineExceeded,
			startup: time.Hour,
			trigger: func(t *testing.T, child *reviewAnyStrategy) (context.Context, context.CancelFunc) {
				caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
				go func() {
					select {
					case <-child.started:
						caller.expire()
					case <-time.After(time.Second):
						t.Error("child did not start")
						caller.expire()
					}
				}()
				return caller, func() {}
			},
		},
		{
			name:    "startup-timeout",
			want:    context.DeadlineExceeded,
			startup: 25 * time.Millisecond,
			trigger: func(*testing.T, *reviewAnyStrategy) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			child := &reviewAnyStrategy{cause: cause, delay: 5 * time.Millisecond, started: make(chan struct{})}
			ctx, cancel := tc.trigger(t, child)
			defer cancel()
			strategy := ForAny(child).WithStartupTimeout(tc.startup)
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- strategy.WaitUntilReady(ctx, &reviewNoopTarget{}) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("ForAny did not collect child result")
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want %v and child cause", err, tc.want)
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
				// Leave the caller unexpired so classification cannot race
				// onto callerCtx.Err() and mislabel a startup timeout.
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("internal startup timeout did not reach child")
				}
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

func TestNonContextCausePreservesNonContextWrappers(t *testing.T) {
	inner := errors.New("connection refused")
	wrapped := fmt.Errorf("dial tcp 127.0.0.1:1: %w", inner)
	got := nonContextCause(wrapped)
	if got != wrapped {
		t.Fatalf("nonContextCause = %v (%T), want original wrapper", got, got)
	}
	if !errors.Is(got, inner) {
		t.Fatalf("nonContextCause = %v, want inner cause", got)
	}
}

type reviewStreamWrapper struct {
	err error
}

func (e *reviewStreamWrapper) Error() string { return "stream wrapper: " + e.err.Error() }
func (e *reviewStreamWrapper) Unwrap() error { return e.err }
func (e *reviewStreamWrapper) Is(target error) bool {
	return target == errReviewStreamSentinel || errors.Is(e.err, target)
}

var errReviewStreamSentinel = errors.New("review stream sentinel")

func TestNonContextCausePreservesWrapperDiagnosticsForMixedContext(t *testing.T) {
	cause := &reviewTypedCause{message: "backend failure"}
	wrapped := fmt.Errorf("follow logs: %w", errors.Join(context.Canceled, cause))
	got := nonContextCause(wrapped)
	if !errors.Is(got, cause) {
		t.Fatalf("nonContextCause = %v, want typed cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("nonContextCause = %v, must not retain canceled branch", got)
	}
	if !strings.Contains(got.Error(), "follow logs:") {
		t.Fatalf("nonContextCause = %v, want wrapper diagnostic text", got)
	}

	op := &net.OpError{Op: "read", Net: "tcp", Err: errors.Join(context.Canceled, cause)}
	nested := fmt.Errorf("follow logs: %w", op)
	got = nonContextCause(nested)
	if !errors.Is(got, cause) {
		t.Fatalf("nested wrapper = %v, want typed cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("nested wrapper = %v, must not retain canceled branch", got)
	}
	if !strings.Contains(got.Error(), "follow logs:") {
		t.Fatalf("nested wrapper = %v, want outer diagnostic text", got)
	}
	var gotOp *net.OpError
	if !errors.As(got, &gotOp) {
		t.Fatalf("nested wrapper = %v, want *net.OpError via As", got)
	}

	custom := &reviewStreamWrapper{err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(custom)
	if !errors.Is(got, cause) {
		t.Fatalf("custom wrapper = %v, want typed cause", got)
	}
	if !errors.Is(got, errReviewStreamSentinel) {
		t.Fatalf("custom wrapper = %v, want sentinel via original Is", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("custom wrapper = %v, must not retain canceled branch", got)
	}
	var gotWrapper *reviewStreamWrapper
	if !errors.As(got, &gotWrapper) {
		t.Fatalf("custom wrapper = %v, want *reviewStreamWrapper via As", got)
	}
}

func TestForLogTimeoutDoesNotRetainStaleContextBranch(t *testing.T) {
	cause := &reviewTypedCause{message: "transient stream failure"}
	cases := []struct {
		name  string
		mixed error
	}{
		{name: "joined", mixed: errors.Join(context.Canceled, cause)},
		{name: "wrapped-join", mixed: fmt.Errorf("follow logs: %w", errors.Join(context.Canceled, cause))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(
					caller,
					&reviewLogDeadlineCauseTarget{cause: tc.mixed, started: started},
				)
			}()
			select {
			case <-started:
				caller.expire()
			case <-time.After(time.Second):
				t.Fatal("log stream cause was not retained before deadline")
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("ForLog did not return after deadline")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want context.DeadlineExceeded", err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want typed stream cause", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, must not retain stale context.Canceled branch", err)
			}
			if tc.name == "wrapped-join" && !strings.Contains(err.Error(), "follow logs:") {
				t.Fatalf("error = %v, want wrapper diagnostic text", err)
			}
		})
	}
}
