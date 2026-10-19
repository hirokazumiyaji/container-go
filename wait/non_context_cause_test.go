package wait

import (
	"context"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type typedCause struct {
	message string
}

func (e *typedCause) Error() string { return e.message }

type noopTarget struct{}

func (*noopTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (*noopTarget) Running(context.Context) (bool, error) { return true, nil }
func (*noopTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*noopTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

type execTarget struct {
	cause       error
	block       bool
	started     chan struct{}
	startedOnce sync.Once
}

func newExecTarget(cause error, block bool) *execTarget {
	return &execTarget{
		cause:   cause,
		block:   block,
		started: make(chan struct{}),
	}
}

func (*execTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *execTarget) Running(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, nil
}
func (*execTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *execTarget) ExecCommand(ctx context.Context, _ []string) (int, error) {
	t.startedOnce.Do(func() { close(t.started) })
	if t.block {
		<-ctx.Done()
	}
	return 0, t.cause
}

func TestPollRetainsCheckErrorWhenContextCompletes(t *testing.T) {
	cause := &typedCause{message: "backend failed at deadline"}
	ctx, cancel := context.WithCancel(context.Background())
	err := poll(ctx, options{startupTimeout: time.Hour}, &noopTarget{}, "wait", func(context.Context) error {
		cancel()
		return cause
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want current check cause", err)
	}
	var typed *typedCause
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want typed check cause", err)
	}
	if got := errors.Unwrap(err); got != context.Canceled {
		t.Fatalf("errors.Unwrap = %v, want context.Canceled", got)
	}
}

func TestPollRetainsCheckErrorAtCallerDeadline(t *testing.T) {
	cause := &typedCause{message: "backend failed at deadline"}
	caller := newManualDeadlineContext(time.Now().Add(time.Hour))
	started := make(chan struct{})
	var startOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- poll(caller, options{startupTimeout: time.Hour}, &noopTarget{}, "wait", func(checkCtx context.Context) error {
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
	cause := &typedCause{message: "readiness backend failure"}
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		target := newExecTarget(cause, true)
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
		caller := newManualDeadlineContext(time.Now().Add(time.Hour))
		target := newExecTarget(cause, false)
		done := make(chan error, 1)
		go func() {
			done <- ForExec([]string{"probe"}).WithStartupTimeout(time.Hour).WithPollInterval(5*time.Millisecond).WaitUntilReady(caller, target)
		}()
		var start time.Time
		select {
		case <-target.started:
			start = time.Now()
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
	cause := &typedCause{message: "scanner backend failure"}
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

type logReadStartTarget struct {
	started chan struct{}
}

func (t *logReadStartTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *logReadStartTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *logReadStartTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	// Signal from the reader's first Read (not FollowLogs) so cancellation
	// cannot precede the scan goroutine observing the stream.
	return &logReadStartCloser{ctx: ctx, started: t.started}, nil
}
func (*logReadStartTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

type logReadStartCloser struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (r *logReadStartCloser) Read([]byte) (int, error) {
	r.once.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (*logReadStartCloser) Close() error { return nil }

func TestForLogCancelSynchronizesOnFirstRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, &logReadStartTarget{started: started})
	}()
	var start time.Time
	select {
	case <-started:
		start = time.Now()
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

type logDeadlineCauseTarget struct {
	cause   error
	started chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (t *logDeadlineCauseTarget) Endpoint(context.Context, string) (string, error) {
	return "", t.cause
}
func (t *logDeadlineCauseTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *logDeadlineCauseTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
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
func (t *logDeadlineCauseTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, t.cause
}

func TestForLogDeadlineRetainsStreamCause(t *testing.T) {
	cause := &typedCause{message: "scanner backend failure"}
	caller := newManualDeadlineContext(time.Now().Add(time.Hour))
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).
			WaitUntilReady(caller, &logDeadlineCauseTarget{cause: cause, started: started})
	}()
	var start time.Time
	select {
	case <-started:
		start = time.Now()
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

type manualDeadlineContext struct {
	deadline time.Time
	done     chan struct{}
}

func newManualDeadlineContext(deadline time.Time) *manualDeadlineContext {
	return &manualDeadlineContext{deadline: deadline, done: make(chan struct{})}
}

func (c *manualDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *manualDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *manualDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (*manualDeadlineContext) Value(any) any { return nil }

func (c *manualDeadlineContext) expire() { close(c.done) }

type logTerminationTarget struct {
	terminate func()
	started   chan struct{}
	once      sync.Once
}

func (t *logTerminationTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (t *logTerminationTarget) Running(context.Context) (bool, error) {
	return true, nil
}
func (t *logTerminationTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.once.Do(func() {
		if t.started != nil {
			close(t.started)
		}
		t.terminate()
	})
	return io.NopCloser(strings.NewReader("")), nil
}
func (*logTerminationTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

func TestForLogDoesNotOutliveCallerTermination(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		target := &logTerminationTarget{terminate: cancel, started: started}
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
		caller := newManualDeadlineContext(time.Now().Add(time.Hour))
		target := &logTerminationTarget{terminate: caller.expire, started: make(chan struct{})}
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

type testAnyStrategy struct {
	cause   error
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func (s *testAnyStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.cause
}

func TestForAnyCollectsContextAwareChildCause(t *testing.T) {
	cause := &typedCause{message: "child backend failure"}
	cases := []struct {
		name    string
		want    error
		startup time.Duration
		trigger func(t *testing.T, child *testAnyStrategy) (context.Context, context.CancelFunc)
	}{
		{
			name:    "caller-cancel",
			want:    context.Canceled,
			startup: time.Hour,
			trigger: func(t *testing.T, child *testAnyStrategy) (context.Context, context.CancelFunc) {
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
			trigger: func(t *testing.T, child *testAnyStrategy) (context.Context, context.CancelFunc) {
				caller := newManualDeadlineContext(time.Now().Add(time.Hour))
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
			trigger: func(*testing.T, *testAnyStrategy) (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			child := &testAnyStrategy{cause: cause, delay: 5 * time.Millisecond, started: make(chan struct{})}
			ctx, cancel := tc.trigger(t, child)
			defer cancel()
			strategy := ForAny(child).WithStartupTimeout(tc.startup)
			done := make(chan error, 1)
			go func() { done <- strategy.WaitUntilReady(ctx, &noopTarget{}) }()
			var start time.Time
			select {
			case <-child.started:
				start = time.Now()
			case err := <-done:
				t.Fatalf("ForAny returned before child started: %v", err)
			case <-time.After(time.Second):
				t.Fatal("child strategy did not start")
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
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("ForAny took %v, want bounded result collection", elapsed)
			}
		})
	}
}

type testReleaseStrategy struct {
	started    chan struct{}
	release    <-chan struct{}
	canceled   chan struct{}
	err        error
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (s *testReleaseStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
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
			caller := newManualDeadlineContext(time.Now().Add(tc.callerDeadline))
			started := make(chan struct{})
			release := make(chan struct{})
			canceled := make(chan struct{})
			childCause := &typedCause{message: "child failure"}
			strategy := ForAll(&testReleaseStrategy{started: started, release: release, canceled: canceled, err: childCause}).
				WithStartupTimeout(tc.startupTimeout)
			done := make(chan error, 1)
			go func() { done <- strategy.WaitUntilReady(caller, &noopTarget{}) }()
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

type multiWrapper struct {
	errs []error
}

func (e *multiWrapper) Error() string {
	return "multi wrapper: " + errors.Join(e.errs...).Error()
}
func (e *multiWrapper) Unwrap() []error {
	out := make([]error, len(e.errs))
	copy(out, e.errs)
	return out
}

type sliceError []string

func (e sliceError) Error() string { return strings.Join(e, ",") }

// structSliceError is a non-comparable struct error (value receiver)
// used to ensure untouched multi-wrappers are not replaced.
type structSliceError struct {
	Parts []string
}

func (e structSliceError) Error() string { return strings.Join(e.Parts, ",") }

// funcBearingError is non-comparable and fails reflect.DeepEqual between
// boxed copies because of the function field.
type funcBearingError struct {
	Label string
	Fn    func()
}

func (e funcBearingError) Error() string { return e.Label }

func TestNonContextCauseHandlesNonComparableErrors(t *testing.T) {
	nc := sliceError{"backend", "failure"}
	joined := errors.Join(nc, errors.New("other"))
	got := nonContextCause(joined)
	if got != joined {
		t.Fatalf("non-comparable join = %v (%T), want original", got, got)
	}
	structChild := structSliceError{Parts: []string{"backend", "failure"}}
	structMulti := &multiWrapper{errs: []error{structChild}}
	got = nonContextCause(structMulti)
	if got != structMulti {
		t.Fatalf("struct-slice multi wrapper = %v (%T), want original", got, got)
	}
	var gotStructMulti *multiWrapper
	if !errors.As(got, &gotStructMulti) || gotStructMulti != structMulti {
		t.Fatalf("struct-slice multi wrapper lost *multiWrapper via As: %v (%T)", got, got)
	}
	funcChild := funcBearingError{Label: "backend", Fn: func() {}}
	funcMulti := &multiWrapper{errs: []error{funcChild}}
	got = nonContextCause(funcMulti)
	if got != funcMulti {
		t.Fatalf("func-bearing multi wrapper = %v (%T), want original", got, got)
	}
	var gotFuncMulti *multiWrapper
	if !errors.As(got, &gotFuncMulti) || gotFuncMulti != funcMulti {
		t.Fatalf("func-bearing multi wrapper lost *multiWrapper via As: %v (%T)", got, got)
	}
	mixed := errors.Join(context.Canceled, nc)
	got = nonContextCause(mixed)
	if errors.Is(got, context.Canceled) {
		t.Fatalf("mixed non-comparable = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("mixed non-comparable = %q, must not render stripped context", got.Error())
	}
	if !errors.Is(got, nc) && got.Error() != nc.Error() {
		// sliceError is not comparable for errors.Is equality by value;
		// at least retain its diagnostic text through the filtered tree.
		if !strings.Contains(got.Error(), "backend") {
			t.Fatalf("mixed non-comparable = %v, want retained diagnostic", got)
		}
	}
}

func TestNonContextCauseRebuildsStdlibJoinWithoutContextText(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	got := nonContextCause(errors.Join(context.Canceled, cause))
	if !errors.Is(got, cause) {
		t.Fatalf("filtered join = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("filtered join = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("filtered join = %q, must not render stripped context", got.Error())
	}
}

func TestNonContextCausePreservesUntouchedMultiErrors(t *testing.T) {
	a := errors.New("backend a")
	b := errors.New("backend b")
	joined := errors.Join(a, b)
	if got := nonContextCause(joined); got != joined {
		t.Fatalf("untouched join = %v (%T), want original", got, got)
	}
	multi := &multiWrapper{errs: []error{a, b}}
	if got := nonContextCause(multi); got != multi {
		t.Fatalf("untouched multi wrapper = %v (%T), want original", got, got)
	}
	emptyMulti := &multiWrapper{errs: nil}
	gotEmpty := nonContextCause(emptyMulti)
	if gotEmpty != emptyMulti {
		t.Fatalf("empty multi wrapper = %v (%T), want original", gotEmpty, gotEmpty)
	}
	var gotEmptyMulti *multiWrapper
	if !errors.As(gotEmpty, &gotEmptyMulti) || gotEmptyMulti != emptyMulti {
		t.Fatalf("empty multi wrapper lost *multiWrapper via As: %v (%T)", gotEmpty, gotEmpty)
	}
	mixed := &multiWrapper{errs: []error{context.Canceled, a}}
	got := nonContextCause(mixed)
	if got == mixed {
		t.Fatal("mixed multi wrapper should be rebuilt")
	}
	if !errors.Is(got, a) || errors.Is(got, context.Canceled) {
		t.Fatalf("mixed multi wrapper = %v", got)
	}
	var gotMulti *multiWrapper
	if errors.As(got, &gotMulti) {
		t.Fatalf("mixed multi wrapper exposed unfiltered *multiWrapper via As")
	}

	onlyContext := &multiWrapper{errs: []error{context.Canceled, context.DeadlineExceeded}}
	got = nonContextCause(onlyContext)
	if got == nil {
		t.Fatal("context-only custom multi wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("context-only multi wrapper = %v, must suppress context", got)
	}
	if errors.As(got, &gotMulti) {
		t.Fatalf("context-only multi wrapper exposed unfiltered *multiWrapper via As")
	}
	if join := errors.Join(context.Canceled, context.DeadlineExceeded); nonContextCause(join) != nil {
		t.Fatalf("stdlib join of only context = %v, want nil", nonContextCause(join))
	}

	onlyContextCustom := &multiWrapper{errs: []error{context.Canceled}}
	got = nonContextCause(onlyContextCustom)
	if got == nil {
		t.Fatal("context-only custom wrapper dropped")
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("context-only custom wrapper = %q, must not render stripped context", got.Error())
	}

	passthrough := &passthroughWrapper{err: context.Canceled}
	got = nonContextCause(passthrough)
	if got == nil {
		t.Fatal("context-only passthrough wrapper dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("context-only passthrough wrapper = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("context-only passthrough wrapper = %q, must not restore stripped text", got.Error())
	}

	dupContextOnly := &multiWrapper{errs: []error{context.Canceled, context.Canceled}}
	got = nonContextCause(dupContextOnly)
	if got == nil {
		t.Fatal("duplicate context-only multi wrapper dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("duplicate context-only multi wrapper = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("duplicate context-only multi wrapper = %q, must not restore duplicate context text", got.Error())
	}

	if got := nonContextCause(&net.OpError{Op: "read", Net: "tcp", Err: context.Canceled}); got != nil {
		t.Fatalf("context-only *net.OpError = %v, want nil", got)
	}
	if got := nonContextCause(&os.PathError{Op: "open", Path: "/tmp/x", Err: context.Canceled}); got != nil {
		t.Fatalf("context-only *os.PathError = %v, want nil", got)
	}
	if got := nonContextCause(&url.Error{Op: "Get", URL: "http://example", Err: context.DeadlineExceeded}); got != nil {
		t.Fatalf("context-only *url.Error = %v, want nil", got)
	}
	if got := nonContextCause(&exec.Error{Name: "docker", Err: context.Canceled}); got != nil {
		t.Fatalf("context-only *exec.Error = %v, want nil", got)
	}
	if got := nonContextCause(&strconv.NumError{Func: "ParseInt", Num: "x", Err: context.Canceled}); got != nil {
		t.Fatalf("context-only *strconv.NumError = %v, want nil", got)
	}
}

func TestNonContextCauseAsDoesNotExposeDiscardedTransportChildren(t *testing.T) {
	backend := &typedCause{message: "backend failure"}
	wrapped := fmt.Errorf("%w; %w", &os.PathError{Op: "open", Path: "/tmp/x", Err: context.Canceled}, backend)
	got := nonContextCause(wrapped)
	if !errors.Is(got, backend) {
		t.Fatalf("filtered wrap = %v, want backend", got)
	}
	var pathErr *os.PathError
	if errors.As(got, &pathErr) {
		t.Fatalf("filtered wrap exposed discarded *os.PathError: %#v", pathErr)
	}
}

// virtualCode is a classification supplied only via a wrapper As method,
// not present on the wrapped cause tree.
type virtualCode struct {
	Code string
}

func (e *virtualCode) Error() string {
	if e == nil {
		return "virtual code"
	}
	return "virtual code: " + e.Code
}

type virtualAsWrapper struct {
	err  error
	code *virtualCode
}

func (e *virtualAsWrapper) Error() string { return "virtual: " + e.err.Error() }
func (e *virtualAsWrapper) Unwrap() error { return e.err }
func (e *virtualAsWrapper) As(target any) bool {
	if t, ok := target.(**virtualCode); ok && e != nil && e.code != nil {
		*t = e.code
		return true
	}
	return false
}

func TestNonContextCausePreservesVirtualAsClassification(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	code := &virtualCode{Code: "E_BACKEND"}
	wrapped := &virtualAsWrapper{err: errors.Join(context.Canceled, cause), code: code}
	got := nonContextCause(wrapped)
	if !errors.Is(got, cause) {
		t.Fatalf("virtual As wrapper = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("virtual As wrapper = %v, must drop canceled", got)
	}
	var gotCode *virtualCode
	if !errors.As(got, &gotCode) || gotCode != code {
		t.Fatalf("virtual As wrapper = %v, want virtual classification via As", got)
	}
	var gotWrapper *virtualAsWrapper
	if errors.As(got, &gotWrapper) {
		t.Fatalf("virtual As wrapper exposed unfiltered *virtualAsWrapper via As")
	}

	// Wrapper and retained child both match *virtualCode: wrapper As
	// must win, matching errors.As depth-first order on the original tree.
	childCode := &virtualCode{Code: "CHILD"}
	wrapperCode := &virtualCode{Code: "WRAP"}
	competing := &virtualAsWrapper{
		err:  errors.Join(context.Canceled, childCode),
		code: wrapperCode,
	}
	got = nonContextCause(competing)
	if !errors.Is(got, childCode) {
		t.Fatalf("competing As wrapper = %v, want child code via Is", got)
	}
	var gotCompeting *virtualCode
	if !errors.As(got, &gotCompeting) || gotCompeting != wrapperCode {
		t.Fatalf("competing As wrapper = %#v, want wrapper code via As precedence", gotCompeting)
	}

	// A custom As that walks unfiltered children must still not expose a
	// discarded context-bearing transport wrapper.
	leaky := &opaqueFacade{
		err: errors.Join(&os.PathError{Op: "open", Path: "/tmp/x", Err: context.Canceled}, cause),
	}
	got = nonContextCause(leaky)
	if !errors.Is(got, cause) {
		t.Fatalf("leaky As facade = %v, want cause", got)
	}
	var leakedPath *os.PathError
	if errors.As(got, &leakedPath) {
		t.Fatalf("leaky As facade exposed discarded *os.PathError: %#v", leakedPath)
	}
}

func TestNonContextCauseClonesTransportWrappersWithFilteredCause(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	pathErr := &os.PathError{Op: "open", Path: "/tmp/x", Err: errors.Join(context.Canceled, cause)}
	got := nonContextCause(pathErr)
	if !errors.Is(got, cause) {
		t.Fatalf("path error = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("path error = %v, must drop canceled", got)
	}
	var gotPath *os.PathError
	if !errors.As(got, &gotPath) {
		t.Fatalf("path error = %v, want *os.PathError", got)
	}
	if errors.Is(gotPath.Err, context.Canceled) {
		t.Fatalf("path error Err = %v, must not retain canceled", gotPath.Err)
	}

	urlErr := &url.Error{Op: "Get", URL: "http://example", Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(urlErr)
	var gotURL *url.Error
	if !errors.As(got, &gotURL) {
		t.Fatalf("url error = %v, want *url.Error", got)
	}
	if errors.Is(gotURL.Err, context.Canceled) {
		t.Fatalf("url error Err = %v, must not retain canceled", gotURL.Err)
	}

	linkErr := &os.LinkError{Op: "link", Old: "/tmp/a", New: "/tmp/b", Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(linkErr)
	var gotLink *os.LinkError
	if !errors.As(got, &gotLink) {
		t.Fatalf("link error = %v, want *os.LinkError", got)
	}
	if errors.Is(gotLink.Err, context.Canceled) {
		t.Fatalf("link error Err = %v, must not retain canceled", gotLink.Err)
	}

	dnsCfg := &net.DNSConfigError{Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(dnsCfg)
	var gotDNSCfg *net.DNSConfigError
	if !errors.As(got, &gotDNSCfg) {
		t.Fatalf("dns config error = %v, want *net.DNSConfigError", got)
	}
	if errors.Is(gotDNSCfg.Err, context.Canceled) {
		t.Fatalf("dns config error Err = %v, must not retain canceled", gotDNSCfg.Err)
	}

	mixedDNS := errors.Join(context.Canceled, cause)
	dnsErr := &net.DNSError{Err: mixedDNS.Error(), Name: "example", UnwrapErr: mixedDNS}
	got = nonContextCause(dnsErr)
	var gotDNS *net.DNSError
	if !errors.As(got, &gotDNS) {
		t.Fatalf("dns error = %v, want *net.DNSError", got)
	}
	if errors.Is(gotDNS.UnwrapErr, context.Canceled) {
		t.Fatalf("dns error UnwrapErr = %v, must not retain canceled", gotDNS.UnwrapErr)
	}
	if strings.Contains(gotDNS.Err, "context canceled") || strings.Contains(gotDNS.Error(), "context canceled") {
		t.Fatalf("dns error = %q (Err=%q), must not render stripped context", gotDNS.Error(), gotDNS.Err)
	}
	if !strings.Contains(gotDNS.Error(), "backend failure") {
		t.Fatalf("dns error = %q, want filtered cause text", gotDNS.Error())
	}

	independentDNS := &net.DNSError{Err: "no such host", Name: "example", UnwrapErr: mixedDNS}
	got = nonContextCause(independentDNS)
	var gotIndependentDNS *net.DNSError
	if !errors.As(got, &gotIndependentDNS) {
		t.Fatalf("independent dns error = %v, want *net.DNSError", got)
	}
	if gotIndependentDNS.Err != "no such host" {
		t.Fatalf("independent dns error Err = %q, want preserved DNS reason", gotIndependentDNS.Err)
	}
	if errors.Is(gotIndependentDNS.UnwrapErr, context.Canceled) {
		t.Fatalf("independent dns UnwrapErr = %v, must not retain canceled", gotIndependentDNS.UnwrapErr)
	}

	// Context-only UnwrapErr with an independent DNS reason must still keep
	// *net.DNSError for As, not disappear as a fully stripped carrier.
	contextOnlyUnwrapDNS := &net.DNSError{
		Err:        "no such host",
		Name:       "example.com",
		IsNotFound: true,
		UnwrapErr:  context.Canceled,
	}
	got = nonContextCause(contextOnlyUnwrapDNS)
	var gotContextOnlyDNS *net.DNSError
	if !errors.As(got, &gotContextOnlyDNS) {
		t.Fatalf("context-only unwrap dns = %v (%T), want *net.DNSError", got, got)
	}
	if gotContextOnlyDNS.Err != "no such host" {
		t.Fatalf("context-only unwrap dns Err = %q, want preserved DNS reason", gotContextOnlyDNS.Err)
	}
	if !gotContextOnlyDNS.IsNotFound {
		t.Fatal("context-only unwrap dns IsNotFound = false, want true")
	}
	if gotContextOnlyDNS.UnwrapErr != nil {
		t.Fatalf("context-only unwrap dns UnwrapErr = %v, want nil", gotContextOnlyDNS.UnwrapErr)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("context-only unwrap dns = %v, must not match canceled", got)
	}
	if got := nonContextCause(&net.DNSError{
		Err:       context.Canceled.Error(),
		Name:      "example.com",
		UnwrapErr: context.Canceled,
	}); got != nil {
		t.Fatalf("dns with only context Err/UnwrapErr = %v, want nil", got)
	}

	numErr := &strconv.NumError{Func: "ParseInt", Num: "x", Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(numErr)
	var gotNum *strconv.NumError
	if !errors.As(got, &gotNum) {
		t.Fatalf("num error = %v, want *strconv.NumError", got)
	}
	if errors.Is(gotNum.Err, context.Canceled) {
		t.Fatalf("num error Err = %v, must not retain canceled", gotNum.Err)
	}

	execErr := &exec.Error{Name: "docker", Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(execErr)
	var gotExec *exec.Error
	if !errors.As(got, &gotExec) {
		t.Fatalf("exec error = %v, want *exec.Error", got)
	}
	if errors.Is(gotExec.Err, context.Canceled) {
		t.Fatalf("exec error Err = %v, must not retain canceled", gotExec.Err)
	}

	marshalErr := &json.MarshalerError{Type: reflect.TypeOf(0), Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(marshalErr)
	var gotMarshal *json.MarshalerError
	if !errors.As(got, &gotMarshal) {
		t.Fatalf("json marshaler error = %v, want *json.MarshalerError", got)
	}
	if errors.Is(gotMarshal.Err, context.Canceled) {
		t.Fatalf("json marshaler error Err = %v, must not retain canceled", gotMarshal.Err)
	}

	csvErr := &csv.ParseError{StartLine: 1, Line: 1, Column: 1, Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(csvErr)
	var gotCSV *csv.ParseError
	if !errors.As(got, &gotCSV) {
		t.Fatalf("csv parse error = %v, want *csv.ParseError", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("csv parse error = %v, must not match canceled at top level", got)
	}
	if errors.Is(gotCSV.Err, context.Canceled) {
		t.Fatalf("csv parse error Err = %v, must not retain canceled", gotCSV.Err)
	}

	execTemplateErr := template.ExecError{Name: "tmpl", Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(execTemplateErr)
	var gotExecTemplate template.ExecError
	if !errors.As(got, &gotExecTemplate) {
		t.Fatalf("template exec error = %v, want template.ExecError", got)
	}
	if errors.Is(gotExecTemplate.Err, context.Canceled) {
		t.Fatalf("template exec error Err = %v, must not retain canceled", gotExecTemplate.Err)
	}

	rootsErr := x509.SystemRootsError{Err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(rootsErr)
	var gotRoots x509.SystemRootsError
	if !errors.As(got, &gotRoots) {
		t.Fatalf("system roots error = %v, want x509.SystemRootsError", got)
	}
	if errors.Is(gotRoots.Err, context.Canceled) {
		t.Fatalf("system roots error Err = %v, must not retain canceled", gotRoots.Err)
	}
}

func TestNonContextCausePreservesFmtMultiWrapFraming(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	wrapped := fmt.Errorf("follow logs: %w; backend: %w", context.Canceled, cause)
	got := nonContextCause(wrapped)
	if !errors.Is(got, cause) {
		t.Fatalf("fmt multi-wrap = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("fmt multi-wrap = %v, must drop canceled", got)
	}
	if got.Error() != "follow logs: backend: backend failure" {
		t.Fatalf("fmt multi-wrap = %q, want preserved wrapper-prefix framing", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("fmt multi-wrap = %q, must not render stripped context", got.Error())
	}

	trailingCanceled := fmt.Errorf("follow logs: %w; cancellation: %w", cause, context.Canceled)
	got = nonContextCause(trailingCanceled)
	if !errors.Is(got, cause) {
		t.Fatalf("trailing labeled canceled = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("trailing labeled canceled = %v, must drop canceled", got)
	}
	if got.Error() != "follow logs: backend failure; cancellation: " &&
		got.Error() != "follow logs: backend failure; cancellation:" {
		t.Fatalf("trailing labeled canceled = %q, want preserved cancellation label", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("trailing labeled canceled = %q, must not render stripped context", got.Error())
	}

	backend := errors.New("peer reported context canceled")
	mixed := fmt.Errorf("stream: %w; backend: %w", context.Canceled, backend)
	got = nonContextCause(mixed)
	if !errors.Is(got, backend) {
		t.Fatalf("fmt multi-wrap with phrase = %v, want backend cause", got)
	}
	if !strings.Contains(got.Error(), "peer reported context canceled") {
		t.Fatalf("fmt multi-wrap with phrase = %q, want intact backend diagnostic", got.Error())
	}
	if strings.HasPrefix(strings.TrimSpace(got.Error()), "context canceled") {
		t.Fatalf("fmt multi-wrap with phrase = %q, discarded sentinel still leads", got.Error())
	}

	sameText := errors.New("context canceled")
	other := errors.New("other backend failure")
	dup := fmt.Errorf("stream: %w; backend: %w; other: %w", context.Canceled, sameText, other)
	got = nonContextCause(dup)
	if !errors.Is(got, sameText) || !errors.Is(got, other) {
		t.Fatalf("fmt multi-wrap with duplicate phrase = %v, want both retained causes", got)
	}
	// Duplicate child text makes framed reconstruction ambiguous; fall back to
	// the filtered cause tree while still suppressing context matches.
	if errors.Is(got, context.Canceled) {
		t.Fatalf("fmt multi-wrap with duplicate phrase = %v, must drop canceled", got)
	}
	if !strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("fmt multi-wrap with duplicate phrase = %q, want retained cause text", got.Error())
	}
	if !strings.Contains(got.Error(), "other backend failure") {
		t.Fatalf("fmt multi-wrap with duplicate phrase = %q, want other cause text", got.Error())
	}

	spaced := errors.New("syntax  near token")
	spacedWrap := fmt.Errorf("stream: %w; backend: %w", context.Canceled, spaced)
	got = nonContextCause(spacedWrap)
	if !strings.Contains(got.Error(), "syntax  near token") {
		t.Fatalf("spaced backend diagnostic = %q, want preserved double space", got.Error())
	}

	ambiguous := fmt.Errorf("backend=%[2]w; ctx=%[1]w", context.Canceled, errors.New("context canceled"))
	got = nonContextCause(ambiguous)
	if errors.Is(got, context.Canceled) {
		t.Fatalf("ambiguous indexed duplicate = %v, must drop canceled", got)
	}
	if got.Error() != "context canceled" {
		// Fall back to the filtered cause text rather than a mis-associated frame.
		if !strings.Contains(got.Error(), "context canceled") || strings.Contains(got.Error(), "backend=") {
			t.Fatalf("ambiguous indexed duplicate = %q, want cause-only fallback", got.Error())
		}
	}

	// Indexed reorder where the discarded sentinel is a substring of the
	// retained backend diagnostic must not claim inside that diagnostic.
	overlapBackend := errors.New("peer reported context canceled")
	overlap := fmt.Errorf("backend=%[2]w; ctx=%[1]w", context.Canceled, overlapBackend)
	got = nonContextCause(overlap)
	if !errors.Is(got, overlapBackend) {
		t.Fatalf("overlapping child text = %v, want backend", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("overlapping child text = %v, must drop canceled", got)
	}
	if !strings.Contains(got.Error(), "peer reported context canceled") {
		t.Fatalf("overlapping child text = %q, want intact backend diagnostic", got.Error())
	}
	if strings.Contains(got.Error(), "ctx=context canceled") {
		t.Fatalf("overlapping child text = %q, discarded sentinel still rendered", got.Error())
	}
	// Corrupted rewrite such as "peer reported ; ctx=context canceled" must not appear.
	if strings.Contains(got.Error(), "peer reported ;") || strings.Contains(got.Error(), "peer reported  ") {
		t.Fatalf("overlapping child text = %q, backend diagnostic corrupted", got.Error())
	}

	// Literal framing that copies a child phrase must not steal the claim.
	framedCause := &typedCause{message: "backend failure"}
	framed := fmt.Errorf("note=context canceled; ctx=%w; backend=%w", context.Canceled, framedCause)
	got = nonContextCause(framed)
	if !errors.Is(got, framedCause) {
		t.Fatalf("ambiguous framing copy = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("ambiguous framing copy = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "ctx=context canceled") {
		t.Fatalf("ambiguous framing copy = %q, discarded sentinel still rendered at ctx=", got.Error())
	}
	// Prefer the filtered cause text over a mis-associated frame rewrite.
	if strings.Contains(got.Error(), "note=") && strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("ambiguous framing copy = %q, want cause-only fallback without framing copies", got.Error())
	}

	nestedCause := &typedCause{message: "nested backend failure"}
	nestedChild := fmt.Errorf("attempt: %w", errors.Join(context.Canceled, nestedCause))
	nested := fmt.Errorf("stream: %w; backend: %w", context.Canceled, nestedChild)
	got = nonContextCause(nested)
	if !errors.Is(got, nestedCause) {
		t.Fatalf("nested filtered child = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("nested filtered child = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("nested filtered child = %q, must render filtered child text", got.Error())
	}
	if !strings.Contains(got.Error(), "nested backend failure") {
		t.Fatalf("nested filtered child = %q, want cause text", got.Error())
	}

	mid := fmt.Errorf("follow %w failed", errors.Join(context.Canceled, cause))
	got = nonContextCause(mid)
	if !strings.Contains(got.Error(), "follow") || !strings.Contains(got.Error(), "failed") {
		t.Fatalf("mid %%w framing = %q, want follow/failed framing", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("mid %%w framing = %q, must not render stripped context", got.Error())
	}

	// Single-%w wrappers that also interpolate the inner via %s must rewrite
	// every copy, not only the suffix/%w occurrence.
	innerJoin := errors.Join(context.Canceled, cause)
	dupSingle := fmt.Errorf("note=%s; wrapped=%w", innerJoin, innerJoin)
	got = nonContextCause(dupSingle)
	if !errors.Is(got, cause) {
		t.Fatalf("duplicated single-wrap inner = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("duplicated single-wrap inner = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("duplicated single-wrap inner = %q, must rewrite every inner copy", got.Error())
	}
	if !strings.Contains(got.Error(), "backend failure") {
		t.Fatalf("duplicated single-wrap inner = %q, want cause text", got.Error())
	}

	delimCause := errors.New("parser saw: ; token")
	delimWrap := fmt.Errorf("stream: %w; backend: %w", context.Canceled, delimCause)
	got = nonContextCause(delimWrap)
	if !strings.Contains(got.Error(), "parser saw: ; token") {
		t.Fatalf("delimiter in backend diagnostic = %q, want intact retained cause", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("delimiter in backend diagnostic = %q, must not render stripped context", got.Error())
	}

	punctCause := errors.New(":backend:")
	punctWrap := fmt.Errorf("%w; %w", context.Canceled, punctCause)
	got = nonContextCause(punctWrap)
	if !errors.Is(got, punctCause) {
		t.Fatalf("punctuated backend diagnostic = %v, want cause", got)
	}
	if !strings.Contains(got.Error(), ":backend:") {
		t.Fatalf("punctuated backend diagnostic = %q, want intact leading/trailing colons", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("punctuated backend diagnostic = %q, must not render stripped context", got.Error())
	}

	// Retained cause ending in a space must not let separator expansion walk
	// backward into that claimed span (overlapping edits would panic).
	spacedTail := errors.New("backend ")
	adjacent := fmt.Errorf("%w%w", spacedTail, context.Canceled)
	got = nonContextCause(adjacent)
	if !errors.Is(got, spacedTail) {
		t.Fatalf("adjacent spaced cause = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("adjacent spaced cause = %v, must drop canceled", got)
	}
	if !strings.Contains(got.Error(), "backend") {
		t.Fatalf("adjacent spaced cause = %q, want retained cause text", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("adjacent spaced cause = %q, must not render stripped context", got.Error())
	}

	// Middle discarded child must not let junction cleanup drop a later cause.
	left := errors.New("x: ")
	right := errors.New("y")
	middle := fmt.Errorf("%w%w;%w", left, context.Canceled, right)
	got = nonContextCause(middle)
	if !errors.Is(got, left) || !errors.Is(got, right) {
		t.Fatalf("middle discarded child = %v, want both retained causes", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("middle discarded child = %v, must drop canceled", got)
	}
	if !strings.Contains(got.Error(), "y") {
		t.Fatalf("middle discarded child = %q, want retained trailing cause", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("middle discarded child = %q, must not render stripped context", got.Error())
	}

	commaCause := errors.New("backend")
	commaWrap := fmt.Errorf("%w, %w", context.Canceled, commaCause)
	got = nonContextCause(commaWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("comma-delimited wrap = %v, want cause", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("comma-delimited wrap = %v, must drop canceled", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("comma-delimited wrap = %q, want absorbed comma framing", got.Error())
	}

	compactComma := fmt.Errorf("%w,%w", context.Canceled, commaCause)
	got = nonContextCause(compactComma)
	if !errors.Is(got, commaCause) {
		t.Fatalf("compact comma wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("compact comma wrap = %q, want absorbed comma without spaces", got.Error())
	}

	newlineWrap := fmt.Errorf("%w\n%w", context.Canceled, commaCause)
	got = nonContextCause(newlineWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("newline-delimited wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("newline-delimited wrap = %q, want absorbed newline framing", got.Error())
	}

	tabWrap := fmt.Errorf("%w\t%w", context.Canceled, commaCause)
	got = nonContextCause(tabWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("tab-delimited wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("tab-delimited wrap = %q, want absorbed tab framing", got.Error())
	}

	pipeWrap := fmt.Errorf("%w | %w", context.Canceled, commaCause)
	got = nonContextCause(pipeWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("pipe-delimited wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("pipe-delimited wrap = %q, want absorbed pipe framing", got.Error())
	}

	compactColon := fmt.Errorf("%w:%w", context.Canceled, commaCause)
	got = nonContextCause(compactColon)
	if !errors.Is(got, commaCause) {
		t.Fatalf("compact colon wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("compact colon wrap = %q, want absorbed colon framing", got.Error())
	}

	compactSemi := fmt.Errorf("%w;%w", context.Canceled, commaCause)
	got = nonContextCause(compactSemi)
	if !errors.Is(got, commaCause) {
		t.Fatalf("compact semicolon wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("compact semicolon wrap = %q, want absorbed semicolon framing", got.Error())
	}

	hyphenWrap := fmt.Errorf("%w - %w", context.Canceled, commaCause)
	got = nonContextCause(hyphenWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("hyphen-delimited wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("hyphen-delimited wrap = %q, want absorbed hyphen framing", got.Error())
	}

	hyphenTrailing := fmt.Errorf("%w - %w", commaCause, context.Canceled)
	got = nonContextCause(hyphenTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing hyphen wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing hyphen wrap = %q, want absorbed trailing hyphen framing", got.Error())
	}

	parenWrap := fmt.Errorf("%w (%w)", context.Canceled, commaCause)
	got = nonContextCause(parenWrap)
	if !errors.Is(got, commaCause) {
		t.Fatalf("paren wrap = %v, want cause", got)
	}
	if got.Error() != "(backend)" {
		t.Fatalf("paren wrap = %q, want intact paired parentheses around retained child", got.Error())
	}

	parenThenArrow := fmt.Errorf("(%w) -> %w", context.Canceled, commaCause)
	got = nonContextCause(parenThenArrow)
	if !errors.Is(got, commaCause) {
		t.Fatalf("paren-then-arrow wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("paren-then-arrow wrap = %q, want pair and arrow framing removed", got.Error())
	}

	bracketDiscarded := fmt.Errorf("[%w] %w", context.Canceled, commaCause)
	got = nonContextCause(bracketDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("bracket-discarded wrap = %q, want balanced brackets removed with discarded child", got.Error())
	}

	bracketTrailing := fmt.Errorf("%w [%w]", commaCause, context.Canceled)
	got = nonContextCause(bracketTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing bracket-discarded wrap = %q, want balanced brackets removed", got.Error())
	}

	bracketSpaced := fmt.Errorf("[ %w ] %w", context.Canceled, commaCause)
	got = nonContextCause(bracketSpaced)
	if !errors.Is(got, commaCause) {
		t.Fatalf("spaced bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("spaced bracket-discarded wrap = %q, want brackets and inner whitespace removed", got.Error())
	}

	bracketSpacedTrailing := fmt.Errorf("%w [ %w ]", commaCause, context.Canceled)
	got = nonContextCause(bracketSpacedTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing spaced bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing spaced bracket-discarded wrap = %q, want brackets and inner whitespace removed", got.Error())
	}

	angleDiscarded := fmt.Errorf("<%w> %w", context.Canceled, commaCause)
	got = nonContextCause(angleDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("angle-bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("angle-bracket-discarded wrap = %q, want balanced angle brackets removed", got.Error())
	}

	angleSpaced := fmt.Errorf("< %w > %w", context.Canceled, commaCause)
	got = nonContextCause(angleSpaced)
	if !errors.Is(got, commaCause) {
		t.Fatalf("spaced angle-bracket-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("spaced angle-bracket-discarded wrap = %q, want angle brackets and inner whitespace removed", got.Error())
	}

	doubleQuoteDiscarded := fmt.Errorf("\"%w\" %w", context.Canceled, commaCause)
	got = nonContextCause(doubleQuoteDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("double-quote-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("double-quote-discarded wrap = %q, want quotes removed with discarded child", got.Error())
	}

	doubleQuoteRetained := fmt.Errorf("%w \"%w\"", context.Canceled, commaCause)
	got = nonContextCause(doubleQuoteRetained)
	if !errors.Is(got, commaCause) {
		t.Fatalf("double-quote retained wrap = %v, want cause", got)
	}
	if got.Error() != "\"backend\"" {
		t.Fatalf("double-quote retained wrap = %q, want quotes around retained child", got.Error())
	}

	singleQuoteDiscarded := fmt.Errorf("'%w' %w", context.Canceled, commaCause)
	got = nonContextCause(singleQuoteDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("single-quote-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("single-quote-discarded wrap = %q, want quotes removed with discarded child", got.Error())
	}

	backtickDiscarded := fmt.Errorf("`%w` %w", context.Canceled, commaCause)
	got = nonContextCause(backtickDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("backtick-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("backtick-discarded wrap = %q, want backticks removed with discarded child", got.Error())
	}

	backtickRetained := fmt.Errorf("%w `%w`", context.Canceled, commaCause)
	got = nonContextCause(backtickRetained)
	if !errors.Is(got, commaCause) {
		t.Fatalf("backtick retained wrap = %v, want cause", got)
	}
	if got.Error() != "`backend`" {
		t.Fatalf("backtick retained wrap = %q, want backticks around retained child", got.Error())
	}

	smartQuoteDiscarded := fmt.Errorf("“%w” %w", context.Canceled, commaCause)
	got = nonContextCause(smartQuoteDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("smart-quote-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("smart-quote-discarded wrap = %q, want smart quotes removed with discarded child", got.Error())
	}

	smartQuoteRetained := fmt.Errorf("%w “%w”", context.Canceled, commaCause)
	got = nonContextCause(smartQuoteRetained)
	if !errors.Is(got, commaCause) {
		t.Fatalf("smart-quote retained wrap = %v, want cause", got)
	}
	if got.Error() != "“backend”" {
		t.Fatalf("smart-quote retained wrap = %q, want smart quotes around retained child", got.Error())
	}

	nestedPairs := fmt.Errorf("[(%w)] %w", context.Canceled, commaCause)
	got = nonContextCause(nestedPairs)
	if !errors.Is(got, commaCause) {
		t.Fatalf("nested-pair-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("nested-pair-discarded wrap = %q, want all enclosing pairs removed", got.Error())
	}

	arrowDiscarded := fmt.Errorf("%w -> %w", context.Canceled, commaCause)
	got = nonContextCause(arrowDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("arrow-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("arrow-discarded wrap = %q, want arrow junction removed", got.Error())
	}

	arrowTrailing := fmt.Errorf("%w -> %w", commaCause, context.Canceled)
	got = nonContextCause(arrowTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing arrow-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing arrow-discarded wrap = %q, want arrow junction removed", got.Error())
	}

	emDashDiscarded := fmt.Errorf("%w — %w", context.Canceled, commaCause)
	got = nonContextCause(emDashDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("em-dash-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("em-dash-discarded wrap = %q, want unicode dash junction removed", got.Error())
	}

	emDashTrailing := fmt.Errorf("%w — %w", commaCause, context.Canceled)
	got = nonContextCause(emDashTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing em-dash-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing em-dash-discarded wrap = %q, want unicode dash junction removed", got.Error())
	}

	unicodeArrowDiscarded := fmt.Errorf("%w → %w", context.Canceled, commaCause)
	got = nonContextCause(unicodeArrowDiscarded)
	if !errors.Is(got, commaCause) {
		t.Fatalf("unicode-arrow-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("unicode-arrow-discarded wrap = %q, want unicode arrow junction removed", got.Error())
	}

	unicodeArrowTrailing := fmt.Errorf("%w → %w", commaCause, context.Canceled)
	got = nonContextCause(unicodeArrowTrailing)
	if !errors.Is(got, commaCause) {
		t.Fatalf("trailing unicode-arrow-discarded wrap = %v, want cause", got)
	}
	if got.Error() != "backend" {
		t.Fatalf("trailing unicode-arrow-discarded wrap = %q, want unicode arrow junction removed", got.Error())
	}

	diskFull := errors.New("disk full")
	adjacentContexts := fmt.Errorf("operation failed: %w, %w; backend: %w", context.Canceled, context.DeadlineExceeded, diskFull)
	got = nonContextCause(adjacentContexts)
	if !errors.Is(got, diskFull) {
		t.Fatalf("adjacent context removals = %v, want disk full", got)
	}
	if errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("adjacent context removals = %v, must drop context", got)
	}
	if got.Error() != "operation failed: backend: disk full" {
		t.Fatalf("adjacent context removals = %q, want merged empty removals keeping framing", got.Error())
	}

	spacedCause := errors.New(" backend ")
	spacedWrap = fmt.Errorf("%w;%w", context.Canceled, spacedCause)
	got = nonContextCause(spacedWrap)
	if !errors.Is(got, spacedCause) {
		t.Fatalf("boundary whitespace cause = %v, want cause", got)
	}
	if got.Error() != " backend " {
		t.Fatalf("boundary whitespace cause = %q, want verbatim retained whitespace", got.Error())
	}

	a := errors.New("alpha failure")
	reordered := fmt.Errorf("ctx=%[2]w; a=%[1]w; b=%[3]w", a, context.Canceled, nestedChild)
	got = nonContextCause(reordered)
	if !errors.Is(got, a) || !errors.Is(got, nestedCause) {
		t.Fatalf("reordered %%w = %v, want retained causes", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("reordered %%w = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("reordered %%w = %q, must not render stripped context", got.Error())
	}
}

type contextMatchingMulti struct {
	errs []error
}

func (e *contextMatchingMulti) Error() string {
	return "context matching multi: " + errors.Join(e.errs...).Error()
}
func (e *contextMatchingMulti) Unwrap() []error {
	out := make([]error, len(e.errs))
	copy(out, e.errs)
	return out
}
func (e *contextMatchingMulti) Is(target error) bool {
	return target == context.Canceled || target == context.DeadlineExceeded
}

type contextMatchingWrapper struct {
	err error
}

func (e *contextMatchingWrapper) Error() string { return "context matching: " + e.err.Error() }
func (e *contextMatchingWrapper) Unwrap() error { return e.err }
func (e *contextMatchingWrapper) Is(target error) bool {
	return target == context.Canceled || errors.Is(e.err, target)
}

// passthroughWrapper renders exactly its child text with no framing.
type passthroughWrapper struct {
	err error
}

func (e *passthroughWrapper) Error() string { return e.err.Error() }
func (e *passthroughWrapper) Unwrap() error { return e.err }

func TestNonContextCauseSuppressesWrapperSuppliedContextMatches(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	multi := &contextMatchingMulti{errs: []error{cause}}
	got := nonContextCause(multi)
	if got == nil {
		t.Fatal("wrapper with non-context children dropped")
	}
	if errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("multi wrapper = %v, must suppress wrapper-supplied context", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("multi wrapper = %v, want child cause", got)
	}

	wrapped := &contextMatchingWrapper{err: cause}
	got = nonContextCause(wrapped)
	if got == nil {
		t.Fatal("single wrapper with non-context child dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("single wrapper = %v, must suppress wrapper-supplied context", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("single wrapper = %v, want child cause", got)
	}
}

type streamWrapper struct {
	err error
}

func (e *streamWrapper) Error() string { return "stream wrapper: " + e.err.Error() }
func (e *streamWrapper) Unwrap() error { return e.err }
func (e *streamWrapper) Is(target error) bool {
	return target == errTestStreamSentinel || errors.Is(e.err, target)
}

var errTestStreamSentinel = errors.New("review stream sentinel")

func TestNonContextCausePreservesWrapperDiagnosticsForMixedContext(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
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
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("nonContextCause = %q, must not render stripped context", got.Error())
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
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("nested wrapper = %q, must not render stripped context", got.Error())
	}
	var gotOp *net.OpError
	if !errors.As(got, &gotOp) {
		t.Fatalf("nested wrapper = %v, want *net.OpError via As", got)
	}

	custom := &streamWrapper{err: errors.Join(context.Canceled, cause)}
	got = nonContextCause(custom)
	if !errors.Is(got, cause) {
		t.Fatalf("custom wrapper = %v, want typed cause", got)
	}
	if !errors.Is(got, errTestStreamSentinel) {
		t.Fatalf("custom wrapper = %v, want sentinel via original Is", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("custom wrapper = %v, must not retain canceled branch", got)
	}
	var gotWrapper *streamWrapper
	if errors.As(got, &gotWrapper) {
		t.Fatalf("custom wrapper exposed unfiltered *streamWrapper via As")
	}
}

// opaqueFacade mirrors cli.safeError: Is/As over an inner tree without
// Unwrap, with UnwrapRaw as the explicit opt-in to the source error.
type opaqueFacade struct {
	err error
}

func (e *opaqueFacade) Error() string { return "opaque: " + e.err.Error() }
func (e *opaqueFacade) Is(target error) bool {
	return e != nil && errors.Is(e.err, target)
}
func (e *opaqueFacade) As(target any) bool {
	return e != nil && errors.As(e.err, target)
}
func (e *opaqueFacade) UnwrapRaw() error {
	if e == nil {
		return nil
	}
	return e.err
}

// repeatedChildTextWrapper unwraps a context sentinel while its Error
// text also embeds that sentinel as independent diagnostic prose.
type repeatedChildTextWrapper struct {
	backend error
	child   error
}

func (e *repeatedChildTextWrapper) Error() string {
	return "peer reported context canceled; context canceled"
}
func (e *repeatedChildTextWrapper) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.child
}
func (e *repeatedChildTextWrapper) Is(target error) bool {
	if e == nil {
		return false
	}
	return errors.Is(e.child, target) || errors.Is(e.backend, target)
}
func (e *repeatedChildTextWrapper) As(target any) bool {
	if e == nil || e.backend == nil {
		return false
	}
	return errors.As(e.backend, target)
}

func TestNonContextCauseKeepsAmbiguousRepeatedSingleChildText(t *testing.T) {
	backend := &typedCause{message: "backend failure"}
	wrapped := &repeatedChildTextWrapper{backend: backend, child: context.Canceled}
	got := nonContextCause(wrapped)
	if got == nil {
		t.Fatal("repeated child-text wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("repeated child-text wrapper = %v, must suppress canceled", got)
	}
	if !errors.Is(got, backend) {
		t.Fatalf("repeated child-text wrapper = %v, want backend via Is", got)
	}
	if got.Error() != "peer reported context canceled; context canceled" {
		t.Fatalf("repeated child-text wrapper = %q, want original safe rendering", got.Error())
	}
	if strings.Contains(got.Error(), "peer reported ;") || got.Error() == "peer reported" {
		t.Fatalf("repeated child-text wrapper = %q, corrupted retained diagnostic", got.Error())
	}

	spaced := &spacedSingleChildWrapper{child: context.Canceled}
	got = nonContextCause(spaced)
	if got == nil {
		t.Fatal("spaced single-child wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("spaced single-child wrapper = %v, must suppress canceled", got)
	}
	if got.Error() == "backend:" || !strings.HasPrefix(got.Error(), " ") {
		t.Fatalf("spaced single-child wrapper = %q, want retained boundary whitespace", got.Error())
	}
	if !strings.Contains(got.Error(), "backend:") {
		t.Fatalf("spaced single-child wrapper = %q, want backend diagnostic", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("spaced single-child wrapper = %q, must strip unique child text", got.Error())
	}

	framedBackend := &typedCause{message: "backend"}
	framed := &framedUnchangedChildWrapper{child: framedBackend}
	got = nonContextCause(framed)
	if got == nil {
		t.Fatal("framed unchanged-child wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("framed unchanged-child wrapper = %v, must suppress canceled", got)
	}
	if !errors.Is(got, framedBackend) {
		t.Fatalf("framed unchanged-child wrapper = %v, want backend via Is", got)
	}
	if got.Error() != "operation failed: backend" {
		t.Fatalf("framed unchanged-child wrapper = %q, want preserved framing", got.Error())
	}
}

// framedUnchangedChildWrapper matches context itself while wrapping an
// unchanged backend child whose Error text already appears in the frame.
type framedUnchangedChildWrapper struct {
	child error
}

func (e *framedUnchangedChildWrapper) Error() string {
	return "operation failed: " + e.child.Error()
}
func (e *framedUnchangedChildWrapper) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.child
}
func (e *framedUnchangedChildWrapper) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == context.Canceled || errors.Is(e.child, target)
}

// spacedSingleChildWrapper keeps intentional boundary whitespace around
// a unique context child rendering.
type spacedSingleChildWrapper struct {
	child error
}

func (e *spacedSingleChildWrapper) Error() string {
	return " backend: context canceled "
}
func (e *spacedSingleChildWrapper) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.child
}

// opaqueIsAsFacade matches context and a backend sentinel via Is/As only.
type opaqueIsAsFacade struct {
	backend error
}

func (e *opaqueIsAsFacade) Error() string { return "opaque is/as facade" }
func (e *opaqueIsAsFacade) Is(target error) bool {
	if e == nil {
		return false
	}
	return errors.Is(target, context.Canceled) || errors.Is(e.backend, target)
}
func (e *opaqueIsAsFacade) As(target any) bool {
	if e == nil || e.backend == nil {
		return false
	}
	return errors.As(e.backend, target)
}

func TestNonContextCauseRetainsOpaqueIsAsWithoutUnwrap(t *testing.T) {
	backend := &typedCause{message: "backend failure"}
	facade := &opaqueIsAsFacade{backend: backend}
	got := nonContextCause(facade)
	if got == nil {
		t.Fatal("opaque Is/As facade dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("opaque Is/As facade = %v, must suppress canceled", got)
	}
	if !errors.Is(got, backend) {
		t.Fatalf("opaque Is/As facade = %v, want backend via Is", got)
	}
	var gotCause *typedCause
	if !errors.As(got, &gotCause) || gotCause != backend {
		t.Fatalf("opaque Is/As facade = %v, want backend via As", got)
	}
	var gotFacade *opaqueIsAsFacade
	if !errors.As(got, &gotFacade) {
		t.Fatalf("opaque Is/As facade = %v, want original type via As", got)
	}
}

type opaqueContextTextFacade struct {
	backend error
}

func (e *opaqueContextTextFacade) Error() string {
	return "context canceled: backend failed"
}
func (e *opaqueContextTextFacade) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == context.Canceled || errors.Is(e.backend, target)
}
func (e *opaqueContextTextFacade) As(target any) bool {
	if e == nil || e.backend == nil {
		return false
	}
	return errors.As(e.backend, target)
}

func TestNonContextCauseStripsOpaqueFacadeContextText(t *testing.T) {
	backend := &typedCause{message: "backend failed"}
	facade := &opaqueContextTextFacade{backend: backend}
	got := nonContextCause(facade)
	if got == nil {
		t.Fatal("opaque context-text facade dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("opaque context-text facade = %v, must suppress canceled", got)
	}
	if !errors.Is(got, backend) {
		t.Fatalf("opaque context-text facade = %v, want backend via Is", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("opaque context-text facade = %q, must strip canceled wording", got.Error())
	}
	if !strings.Contains(got.Error(), "backend failed") {
		t.Fatalf("opaque context-text facade = %q, want backend diagnostic text", got.Error())
	}

	ambiguous := &opaqueAmbiguousContextTextFacade{backend: backend}
	got = nonContextCause(ambiguous)
	if got == nil {
		t.Fatal("ambiguous opaque facade dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("ambiguous opaque facade = %v, must suppress canceled", got)
	}
	if !errors.Is(got, backend) {
		t.Fatalf("ambiguous opaque facade = %v, want backend via Is", got)
	}
	// Ambiguous repeated sentinel phrases fall back to empty rather than
	// corrupting the legitimate "peer reported context canceled" wording.
	if strings.Contains(got.Error(), "peer reported") && !strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("ambiguous opaque facade = %q, corrupted retained phrase", got.Error())
	}
	if got.Error() == "peer reported" {
		t.Fatalf("ambiguous opaque facade = %q, stripped inside retained diagnostic", got.Error())
	}

	embedded := &opaqueEmbeddedContextTextFacade{backend: backend}
	got = nonContextCause(embedded)
	if got == nil {
		t.Fatal("embedded opaque facade dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("embedded opaque facade = %v, must suppress canceled", got)
	}
	if !errors.Is(got, backend) {
		t.Fatalf("embedded opaque facade = %v, want backend via Is", got)
	}
	if got.Error() == "peer reported" {
		t.Fatalf("embedded opaque facade = %q, stripped legitimate diagnostic text", got.Error())
	}
	if got.Error() != "" && strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("embedded opaque facade = %q, must not keep canceled wording", got.Error())
	}
}

type opaqueAmbiguousContextTextFacade struct {
	backend error
}

func (e *opaqueAmbiguousContextTextFacade) Error() string {
	return "context canceled: peer reported context canceled"
}
func (e *opaqueAmbiguousContextTextFacade) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == context.Canceled || errors.Is(e.backend, target)
}
func (e *opaqueAmbiguousContextTextFacade) As(target any) bool {
	if e == nil || e.backend == nil {
		return false
	}
	return errors.As(e.backend, target)
}

type opaqueEmbeddedContextTextFacade struct {
	backend error
}

func (e *opaqueEmbeddedContextTextFacade) Error() string {
	return "peer reported context canceled"
}
func (e *opaqueEmbeddedContextTextFacade) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == context.Canceled || errors.Is(e.backend, target)
}
func (e *opaqueEmbeddedContextTextFacade) As(target any) bool {
	if e == nil || e.backend == nil {
		return false
	}
	return errors.As(e.backend, target)
}

func TestNonContextCauseRetainsOpaqueFacadeNonContextSurface(t *testing.T) {
	cause := &typedCause{message: "logs follow exited 1"}
	opaque := &opaqueFacade{err: errors.Join(context.Canceled, cause)}
	got := nonContextCause(opaque)
	if got == nil {
		t.Fatal("opaque mixed facade dropped entirely")
	}
	if !errors.Is(got, cause) {
		t.Fatalf("opaque facade = %v, want typed cause via Is", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("opaque facade = %v, must suppress canceled", got)
	}
	var gotCause *typedCause
	if !errors.As(got, &gotCause) || gotCause != cause {
		t.Fatalf("opaque facade = %v, want typed cause via As", got)
	}
	var gotFacade *opaqueFacade
	if !errors.As(got, &gotFacade) {
		t.Fatalf("opaque facade = %v, want original facade via As", got)
	}
	if !strings.Contains(got.Error(), "opaque:") {
		t.Fatalf("opaque facade = %v, want original diagnostic text", got)
	}

	if got := nonContextCause(&opaqueFacade{err: context.Canceled}); got != nil {
		t.Fatalf("context-only opaque facade = %v, want nil", got)
	}
	if got := nonContextCause(&opaqueFacade{err: errors.Join(context.Canceled, context.DeadlineExceeded)}); got != nil {
		t.Fatalf("context-only opaque join = %v, want nil", got)
	}
}

// sanitizingWrapper unwraps a raw cause tree but never renders it.
type sanitizingWrapper struct {
	err error
}

func (e *sanitizingWrapper) Error() string { return "operation failed" }
func (e *sanitizingWrapper) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func TestNonContextCausePreservesSanitizedWrapperError(t *testing.T) {
	secret := "super-secret-token"
	wrapped := &sanitizingWrapper{err: errors.Join(context.Canceled, errors.New(secret))}
	got := nonContextCause(wrapped)
	if got == nil {
		t.Fatal("sanitizing wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("sanitizing wrapper = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("sanitizing wrapper Error leaked secret: %q", got.Error())
	}
	if got.Error() != "operation failed" {
		t.Fatalf("sanitizing wrapper = %q, want original safe rendering", got.Error())
	}

	multi := &sanitizingMultiWrapper{errs: []error{context.Canceled, errors.New(secret)}, msg: "operation failed"}
	got = nonContextCause(multi)
	if got == nil {
		t.Fatal("sanitizing multi wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("sanitizing multi wrapper = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("sanitizing multi wrapper Error leaked secret: %q", got.Error())
	}
	if got.Error() != "operation failed" {
		t.Fatalf("sanitizing multi wrapper = %q, want original safe rendering", got.Error())
	}

	partial := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("public"), errors.New(secret)},
		msg:  "operation failed: public",
	}
	got = nonContextCause(partial)
	if got == nil {
		t.Fatal("partial sanitizing multi wrapper dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("partial sanitizing multi wrapper = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("partial sanitizing multi wrapper Error leaked secret: %q", got.Error())
	}
	if got.Error() != "operation failed: public" {
		t.Fatalf("partial sanitizing multi wrapper = %q, want original safe rendering", got.Error())
	}

	partialWithContext := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("public"), errors.New(secret)},
		msg:  "context canceled: public",
	}
	got = nonContextCause(partialWithContext)
	if got == nil {
		t.Fatal("partial sanitizing multi wrapper with context text dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("partial sanitizing with context text = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("partial sanitizing with context text leaked secret: %q", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("partial sanitizing with context text = %q, must strip discarded context", got.Error())
	}
	if !strings.Contains(got.Error(), "public") {
		t.Fatalf("partial sanitizing with context text = %q, want retained public text", got.Error())
	}

	overlapPartial := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("peer reported context canceled"), errors.New(secret)},
		msg:  "peer reported context canceled",
	}
	got = nonContextCause(overlapPartial)
	if got == nil {
		t.Fatal("overlap partial sanitizing wrapper dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("overlap partial sanitizing = %v, must suppress canceled", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("overlap partial sanitizing leaked secret: %q", got.Error())
	}
	if got.Error() != "peer reported context canceled" {
		t.Fatalf("overlap partial sanitizing = %q, want intact retained diagnostic", got.Error())
	}

	delimPartial := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("parser saw: ; token"), errors.New(secret)},
		msg:  "context canceled; parser saw: ; token",
	}
	got = nonContextCause(delimPartial)
	if got == nil {
		t.Fatal("delimiter partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("delimiter partial sanitizing leaked secret: %q", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("delimiter partial sanitizing = %q, must strip discarded context", got.Error())
	}
	if !strings.Contains(got.Error(), "parser saw: ; token") {
		t.Fatalf("delimiter partial sanitizing = %q, want intact retained delimiters", got.Error())
	}

	ambiguousPartial := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("public"), errors.New(secret)},
		msg:  "note=context canceled; ctx=context canceled; public",
	}
	got = nonContextCause(ambiguousPartial)
	if got == nil {
		t.Fatal("ambiguous partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("ambiguous partial sanitizing leaked secret: %q", got.Error())
	}
	// Ambiguous framing copies fall back to the original safe text rather than
	// removing the wrong occurrence.
	if got.Error() != "note=context canceled; ctx=context canceled; public" {
		t.Fatalf("ambiguous partial sanitizing = %q, want original safe rendering", got.Error())
	}

	spacedPartial := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New(" backend "), errors.New(secret)},
		msg:  "context canceled;  backend ",
	}
	got = nonContextCause(spacedPartial)
	if got == nil {
		t.Fatal("spaced partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("spaced partial sanitizing leaked secret: %q", got.Error())
	}
	if got.Error() != " backend " {
		t.Fatalf("spaced partial sanitizing = %q, want retained leading whitespace", got.Error())
	}

	adjacentDiscarded := &sanitizingMultiWrapper{
		errs: []error{
			context.Canceled,
			context.DeadlineExceeded,
			errors.New("public"),
			errors.New(secret),
		},
		msg: "context canceled; context deadline exceeded; public",
	}
	got = nonContextCause(adjacentDiscarded)
	if got == nil {
		t.Fatal("adjacent discarded partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("adjacent discarded partial sanitizing leaked secret: %q", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") || strings.Contains(got.Error(), "context deadline exceeded") {
		t.Fatalf("adjacent discarded partial sanitizing = %q, must strip both context spans", got.Error())
	}
	if got.Error() != "public" {
		t.Fatalf("adjacent discarded partial sanitizing = %q, want intact retained diagnostic", got.Error())
	}

	mixedDup := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("context canceled"), errors.New(secret)},
		msg:  "context canceled",
	}
	got = nonContextCause(mixedDup)
	if got == nil {
		t.Fatal("mixed duplicate partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("mixed duplicate partial sanitizing leaked secret: %q", got.Error())
	}
	// Shared phrase between discarded and retained children is ambiguous.
	if got.Error() != "context canceled" {
		t.Fatalf("mixed duplicate partial sanitizing = %q, want original safe rendering", got.Error())
	}

	overlapDup := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("canceled"), errors.New(secret)},
		msg:  "context canceled",
	}
	got = nonContextCause(overlapDup)
	if got == nil {
		t.Fatal("overlapping retained phrase partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("overlapping retained phrase partial sanitizing leaked secret: %q", got.Error())
	}
	if got.Error() != "context canceled" {
		t.Fatalf("overlapping retained phrase partial sanitizing = %q, want original safe rendering", got.Error())
	}

	coalescedDup := &sanitizingMultiWrapper{
		errs: []error{context.Canceled, errors.New("public"), errors.New("public")},
		msg:  "context canceled; public",
	}
	got = nonContextCause(coalescedDup)
	if got == nil {
		t.Fatal("coalesced duplicate partial sanitizing wrapper dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("coalesced duplicate partial sanitizing = %v, must drop canceled", got)
	}
	if got.Error() != "public" {
		t.Fatalf("coalesced duplicate partial sanitizing = %q, want framed coalesced retained text", got.Error())
	}
	if strings.Count(got.Error(), "public") != 1 {
		t.Fatalf("coalesced duplicate partial sanitizing = %q, must not expand coalesced duplicates", got.Error())
	}

	nestedBackend := &typedCause{message: "backend"}
	nestedMixed := errors.Join(context.Canceled, nestedBackend)
	// One changed retained child rendered twice (plus an omitted secret): the
	// extra copy is ambiguous framing and must not leave a stale context span.
	dupNestedPartial := &sanitizingMultiWrapper{
		errs: []error{nestedMixed, errors.New(secret)},
		msg:  nestedMixed.Error() + "; " + nestedMixed.Error(),
	}
	got = nonContextCause(dupNestedPartial)
	if got == nil {
		t.Fatal("duplicate nested mixed partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("duplicate nested mixed partial sanitizing leaked secret: %q", got.Error())
	}
	if got.Error() != nestedMixed.Error()+"; "+nestedMixed.Error() {
		t.Fatalf("duplicate nested mixed partial sanitizing = %q, want original safe rendering", got.Error())
	}

	nestedPartial := &sanitizingMultiWrapper{
		errs: []error{nestedMixed, errors.New(secret)},
		msg:  nestedMixed.Error(),
	}
	got = nonContextCause(nestedPartial)
	if got == nil {
		t.Fatal("nested mixed partial sanitizing wrapper dropped")
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("nested mixed partial sanitizing leaked secret: %q", got.Error())
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("nested mixed partial sanitizing = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("nested mixed partial sanitizing = %q, must rewrite nested mixed child", got.Error())
	}
	if !strings.Contains(got.Error(), "backend") {
		t.Fatalf("nested mixed partial sanitizing = %q, want filtered backend text", got.Error())
	}
}

func TestIsStdlibPackagePathAllowlist(t *testing.T) {
	for _, path := range []string{"os/exec", "strconv", "encoding/json", "encoding/csv", "text/template", "crypto/x509", "net", "errors", "fmt"} {
		if !isStdlibPackagePath(path) {
			t.Fatalf("isStdlibPackagePath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"corp/errors",
		"net/corp",
		"text/errors",
		"github.com/hirokazumiyaji/container-go/wait",
		"example.com/lib",
	} {
		if isStdlibPackagePath(path) {
			t.Fatalf("isStdlibPackagePath(%q) = true, want false", path)
		}
	}
}

type sanitizingMultiWrapper struct {
	errs []error
	msg  string
}

func (e *sanitizingMultiWrapper) Error() string { return e.msg }
func (e *sanitizingMultiWrapper) Unwrap() []error {
	out := make([]error, len(e.errs))
	copy(out, e.errs)
	return out
}

func TestNonContextCauseRetainsRedactorOnFilteredCause(t *testing.T) {
	secret := "super-secret-token"
	cliErr := &cli.CLIError{
		Args:     []string{"logs", "--follow", "ctr"},
		ExitCode: 1,
		Stderr:   "follow failed: " + secret,
	}
	opaque := cli.WithRedactor(errors.Join(context.Canceled, cliErr), cli.NewRedactor(secret))
	got := nonContextCause(opaque)
	if got == nil {
		t.Fatal("redacted mixed facade dropped entirely")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("redacted facade = %v, must suppress canceled", got)
	}
	if !errors.Is(got, cliErr) {
		t.Fatalf("redacted facade = %v, want CLIError via Is", got)
	}
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("redacted facade Error leaked secret: %q", got.Error())
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("redacted facade = %q, must not render stripped context", got.Error())
	}
	var gotCLI *cli.CLIError
	if !errors.As(got, &gotCLI) {
		t.Fatalf("redacted facade = %v, want *cli.CLIError via As", got)
	}
	if strings.Contains(gotCLI.Error(), secret) {
		t.Fatalf("errors.As CLIError leaked secret: %q", gotCLI.Error())
	}
}

func TestForLogTimeoutDoesNotRetainStaleContextBranch(t *testing.T) {
	cause := &typedCause{message: "transient stream failure"}
	cases := []struct {
		name  string
		mixed error
	}{
		{name: "joined", mixed: errors.Join(context.Canceled, cause)},
		{name: "wrapped-join", mixed: fmt.Errorf("follow logs: %w", errors.Join(context.Canceled, cause))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller := newManualDeadlineContext(time.Now().Add(time.Hour))
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(
					caller,
					&logDeadlineCauseTarget{cause: tc.mixed, started: started},
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

func TestFinalLifecycleProbeStripsJoinedContext(t *testing.T) {
	target := &finalProbeJoinedContextTarget{}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected final lifecycle probe error")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
}

func TestFinalLifecycleRetriesContextOnlyAfterTransientProbe(t *testing.T) {
	target := &finalProbeStaleTransientTarget{}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady = %v, want success after retrying context-only permanent probe", err)
	}
	if target.probes.Load() < 3 {
		t.Fatalf("lifecycle probes = %d, want transient then context-only then success", target.probes.Load())
	}
}

func TestCompositeFinalLifecycleProbeStripsJoinedContext(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&compositeMarkDoneStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&compositeMarkDoneStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			target := &compositeFinalProbeJoinedContextTarget{}
			err := strategy.WaitUntilReady(context.Background(), target)
			if err == nil {
				t.Fatal("expected composite final lifecycle probe error")
			}
			if !errors.Is(err, ErrTargetNotFound) {
				t.Fatalf("error = %v, want ErrTargetNotFound", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, must not match canceled while caller is active", err)
			}
		})
	}
}

func TestCompositeFinalLifecycleRetriesContextOnlyProbe(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&compositeContextOnlyProbeStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&compositeContextOnlyProbeStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			target := &compositeContextOnlyProbeTarget{}
			err := strategy.WaitUntilReady(context.Background(), target)
			if err != nil {
				t.Fatalf("WaitUntilReady = %v, want success after context-only final probe retries", err)
			}
			if target.probes.Load() < 2 {
				t.Fatalf("lifecycle probes = %d, want context-only then success", target.probes.Load())
			}
		})
	}
}

func TestCompositeFinalLifecycleCheckBoundedWithoutStartupTimeout(t *testing.T) {
	old := finalLifecycleProbeTimeout
	finalLifecycleProbeTimeout = 200 * time.Millisecond
	defer func() { finalLifecycleProbeTimeout = old }()

	for name, strategy := range map[string]Strategy{
		"all": ForAll(&compositeAlwaysTransientStrategy{}),
		"any": ForAny(&compositeAlwaysTransientStrategy{}),
	} {
		t.Run(name, func(t *testing.T) {
			target := &compositeAlwaysTransientTarget{}
			start := time.Now()
			err := strategy.WaitUntilReady(context.Background(), target)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("expected bounded final lifecycle failure")
			}
			if elapsed > time.Second {
				t.Fatalf("WaitUntilReady took %v, want bounded by shortened finalLifecycleProbeTimeout", elapsed)
			}
			if !strings.Contains(err.Error(), "final lifecycle check") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want final lifecycle check deadline", err)
			}
		})
	}
}

func TestCompositeFinalLifecyclePermanentCheckErrorFailsFast(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&compositeContainerNotFoundStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&compositeContainerNotFoundStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			target := &compositeContainerNotFoundTarget{}
			start := time.Now()
			err := strategy.WaitUntilReady(context.Background(), target)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("expected permanent final lifecycle failure")
			}
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("error = %v, want ErrContainerNotFound", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, must fail immediately without startup deadline", err)
			}
			if elapsed > 300*time.Millisecond {
				t.Fatalf("WaitUntilReady took %v, want immediate permanent failure", elapsed)
			}
		})
	}
}

func TestFinalLifecycleProbeDeadlineRaceStripsCanceled(t *testing.T) {
	backend := &typedCause{message: "backend inspect failed"}
	target := &finalProbeDeadlineRaceTarget{backend: backend}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(80*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected timeout or probe error")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled on deadline race", err)
	}
	if !errors.Is(err, backend) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want backend cause and/or deadline", err)
	}
}

func TestFinalLifecycleProbeTimeoutRetriesInsteadOfUnknownState(t *testing.T) {
	target := &finalProbeTimeoutOnlyTarget{}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(250*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after probe retries")
	}
	if strings.Contains(err.Error(), "final lifecycle state unknown") {
		t.Fatalf("error = %v, must retry probe-context deadline instead of unknown-state failure", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want startup deadline after retries", err)
	}
}

type finalProbeTimeoutOnlyTarget struct {
	execDone atomic.Bool
}

func (*finalProbeTimeoutOnlyTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalProbeTimeoutOnlyTarget) Running(ctx context.Context) (bool, error) {
	if !t.execDone.Load() {
		return true, nil
	}
	<-ctx.Done()
	return false, ctx.Err()
}
func (*finalProbeTimeoutOnlyTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalProbeTimeoutOnlyTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

type finalProbeDeadlineRaceTarget struct {
	backend  error
	execDone atomic.Bool
}

func (*finalProbeDeadlineRaceTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalProbeDeadlineRaceTarget) Running(ctx context.Context) (bool, error) {
	if !t.execDone.Load() {
		return true, nil
	}
	<-ctx.Done()
	return false, errors.Join(context.Canceled, t.backend, ctx.Err())
}
func (*finalProbeDeadlineRaceTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalProbeDeadlineRaceTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

type finalProbeJoinedContextTarget struct {
	execDone atomic.Bool
}

func (*finalProbeJoinedContextTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalProbeJoinedContextTarget) Running(context.Context) (bool, error) {
	if t.execDone.Load() {
		return false, errors.Join(context.Canceled, ErrTargetNotFound)
	}
	return true, nil
}
func (*finalProbeJoinedContextTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalProbeJoinedContextTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

type finalProbeStaleTransientTarget struct {
	execDone atomic.Bool
	probes   atomic.Int32
}

func (*finalProbeStaleTransientTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalProbeStaleTransientTarget) Running(context.Context) (bool, error) {
	if !t.execDone.Load() {
		return true, nil
	}
	switch t.probes.Add(1) {
	case 1:
		return false, errors.New("transient inspect glitch")
	case 2:
		return false, &exec.Error{Name: "container", Err: context.Canceled}
	default:
		return true, nil
	}
}
func (*finalProbeStaleTransientTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalProbeStaleTransientTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

type compositeFinalProbeJoinedContextTarget struct {
	childDone atomic.Bool
}

func (*compositeFinalProbeJoinedContextTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeFinalProbeJoinedContextTarget) Running(context.Context) (bool, error) {
	if t.childDone.Load() {
		return false, errors.Join(context.Canceled, ErrTargetNotFound)
	}
	return true, nil
}
func (*compositeFinalProbeJoinedContextTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeFinalProbeJoinedContextTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type compositeMarkDoneStrategy struct{}

func (*compositeMarkDoneStrategy) WaitUntilReady(_ context.Context, target Target) error {
	if t, ok := target.(*compositeFinalProbeJoinedContextTarget); ok {
		t.childDone.Store(true)
	}
	return nil
}

type compositeContextOnlyProbeTarget struct {
	childDone atomic.Bool
	probes    atomic.Int32
}

func (*compositeContextOnlyProbeTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeContextOnlyProbeTarget) Running(context.Context) (bool, error) {
	if !t.childDone.Load() {
		return true, nil
	}
	if t.probes.Add(1) == 1 {
		return false, context.Canceled
	}
	return true, nil
}
func (*compositeContextOnlyProbeTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeContextOnlyProbeTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type compositeContextOnlyProbeStrategy struct{}

func (*compositeContextOnlyProbeStrategy) WaitUntilReady(_ context.Context, target Target) error {
	if t, ok := target.(*compositeContextOnlyProbeTarget); ok {
		t.childDone.Store(true)
	}
	return nil
}

type compositeAlwaysTransientTarget struct {
	childDone atomic.Bool
}

func (*compositeAlwaysTransientTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeAlwaysTransientTarget) Running(context.Context) (bool, error) {
	if !t.childDone.Load() {
		return true, nil
	}
	return false, errors.New("transient inspect glitch")
}
func (*compositeAlwaysTransientTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeAlwaysTransientTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type compositeAlwaysTransientStrategy struct{}

func (*compositeAlwaysTransientStrategy) WaitUntilReady(_ context.Context, target Target) error {
	if t, ok := target.(*compositeAlwaysTransientTarget); ok {
		t.childDone.Store(true)
	}
	return nil
}

type compositeContainerNotFoundTarget struct {
	childDone atomic.Bool
}

func (*compositeContainerNotFoundTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeContainerNotFoundTarget) Running(context.Context) (bool, error) {
	if !t.childDone.Load() {
		return true, nil
	}
	return false, ErrContainerNotFound
}
func (*compositeContainerNotFoundTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeContainerNotFoundTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type compositeContainerNotFoundStrategy struct{}

func (*compositeContainerNotFoundStrategy) WaitUntilReady(_ context.Context, target Target) error {
	if t, ok := target.(*compositeContainerNotFoundTarget); ok {
		t.childDone.Store(true)
	}
	return nil
}

func TestForExecTimeoutStripsJoinedContext(t *testing.T) {
	backend := &typedCause{message: "backend probe failed"}
	target := newFakeTarget()
	target.execCode = 1
	target.execErr = errors.Join(context.Canceled, backend)

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(200*time.Millisecond).
		WithPollInterval(20*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, backend) {
		t.Fatalf("error = %v, want retained backend cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
}

func TestForExecContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &exec.Error{Name: "container", Err: context.Canceled}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for context-only permanent launch error", target.execCalls.Load())
	}
}

func TestForExecWrappedContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after wrapped context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for wrapped context-only permanent launch error", target.execCalls.Load())
	}
}

func TestForExecNestedWrappedContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = fmt.Errorf("outer: %w", &launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}})

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after nested wrapped context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for nested wrapped context-only permanent launch error", target.execCalls.Load())
	}
}

func TestRetainedNonContextCausePeelsRedactorOverEmptyStripped(t *testing.T) {
	// Redacting a context-only permanent launch wrapper filters to
	// contextStrippedCause → safeError → empty stripped. Peel UnwrapRaw so
	// the chain is treated as context-only for permanent control flow.
	err := cli.WithRedactor(
		&launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}},
		cli.NewRedactor("secret"),
	)
	if got := retainedNonContextCause(err); got != nil {
		t.Fatalf("retainedNonContextCause = %v (%T), want nil under redactor", got, got)
	}
}

func TestForExecRedactedContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = cli.WithRedactor(
		&launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}},
		cli.NewRedactor("secret"),
	)

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after redacted context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for redacted context-only permanent launch error", target.execCalls.Load())
	}
}

func TestRetainedNonContextCausePeelsPathErrorOverEmptyStripped(t *testing.T) {
	// Cloned *os.PathError around an empty stripped facade must peel Unwrap
	// so context-only permanent launch still retries.
	err := &os.PathError{
		Op:   "open",
		Path: "/probe",
		Err:  &launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}},
	}
	if got := retainedNonContextCause(err); got != nil {
		t.Fatalf("retainedNonContextCause = %v (%T), want nil under PathError", got, got)
	}
}

func TestForExecPathErrorContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &os.PathError{
		Op:   "open",
		Path: "/probe",
		Err:  &launchWrapper{err: &exec.Error{Name: "container", Err: context.Canceled}},
	}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after PathError context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for PathError context-only permanent launch error", target.execCalls.Load())
	}
}

func TestRetainedNonContextCauseKeepsIndependentDNSAfterPeel(t *testing.T) {
	// Permanent via *exec.Error in UnwrapErr, but after stripping the retained
	// leaf is *net.DNSError with nil UnwrapErr. Peeling must not discard it.
	err := &net.DNSError{
		Err:        "no such host",
		Name:       "example.com",
		IsNotFound: true,
		UnwrapErr:  &exec.Error{Name: "container", Err: context.Canceled},
	}
	got := retainedNonContextCause(err)
	var dns *net.DNSError
	if !errors.As(got, &dns) {
		t.Fatalf("retained = %v (%T), want *net.DNSError", got, got)
	}
	if dns.Err != "no such host" {
		t.Fatalf("retained DNS Err = %q, want independent reason", dns.Err)
	}
	if dns.UnwrapErr != nil {
		t.Fatalf("retained DNS UnwrapErr = %v, want nil", dns.UnwrapErr)
	}
}

func TestForExecIndependentDNSAfterContextLaunchFailsFast(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &net.DNSError{
		Err:        "no such host",
		Name:       "example.com",
		IsNotFound: true,
		UnwrapErr:  &exec.Error{Name: "container", Err: context.Canceled},
	}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected independent DNS failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want immediate DNS failure, not startup timeout", err)
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("error = %v, want independent DNS reason in message", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("execCalls = %d, want immediate permanent failure without retries", target.execCalls.Load())
	}
}

func TestRetainedNonContextCauseJoinedEmptyStrippedFacades(t *testing.T) {
	err := errors.Join(
		&launchWrapper{err: &exec.Error{Name: "a", Err: context.Canceled}},
		&launchWrapper{err: &exec.Error{Name: "b", Err: context.Canceled}},
	)
	if got := retainedNonContextCause(err); got != nil {
		t.Fatalf("retainedNonContextCause = %v (%T), want nil for joined empty facades", got, got)
	}
}

func TestForExecJoinedContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = errors.Join(
		&launchWrapper{err: &exec.Error{Name: "a", Err: context.Canceled}},
		&launchWrapper{err: &exec.Error{Name: "b", Err: context.Canceled}},
	)

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after joined context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("execCalls = %d, want retries for joined context-only permanent launch error", target.execCalls.Load())
	}
}

func TestForExecOpaquePermanentClassificationFailsFast(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &opaquePermanentFacade{}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected permanent opaque failure")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound via opaque classification", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("execCalls = %d, want immediate permanent failure without retries", target.execCalls.Load())
	}
}

func TestRetainedNonContextCauseNestedPermanentFacade(t *testing.T) {
	// Outer permanent Is over an inner custom wrapper of only context.Canceled
	// filters to nested contextStrippedCause values. Permanence must be checked
	// on every facade, not only the empty leaf.
	inner := &launchWrapper{err: context.Canceled}
	outer := &nestedPermanentFacade{err: inner}

	got := retainedNonContextCause(outer)
	if got == nil {
		t.Fatal("retainedNonContextCause = nil, want outer permanent facade")
	}
	if !errors.Is(got, ErrTargetNotFound) {
		t.Fatalf("retained = %v, want ErrTargetNotFound", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("retained = %v, must not match canceled", got)
	}
}

func TestForExecNestedPermanentFacadeFailsFast(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &nestedPermanentFacade{
		err: &launchWrapper{err: context.Canceled},
	}

	err := ForExec([]string{"probe"}).
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected permanent nested facade failure")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound via outer permanent facade", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("execCalls = %d, want immediate permanent failure without retries", target.execCalls.Load())
	}
}

type opaquePermanentFacade struct{}

func (*opaquePermanentFacade) Error() string { return "opaque permanent" }
func (*opaquePermanentFacade) Is(target error) bool {
	return target == context.Canceled || target == ErrTargetNotFound
}

type nestedPermanentFacade struct {
	err error
}

func (e *nestedPermanentFacade) Error() string {
	return "nested permanent: " + e.err.Error()
}
func (e *nestedPermanentFacade) Unwrap() error { return e.err }
func (e *nestedPermanentFacade) Is(target error) bool {
	return target == ErrTargetNotFound
}

type launchWrapper struct {
	err error
}

func (e *launchWrapper) Error() string { return "launch: " + e.err.Error() }
func (e *launchWrapper) Unwrap() error { return e.err }

func TestForLogTerminalStreamStripsJoinedContext(t *testing.T) {
	terminal := &cli.CLIError{
		Args:     []string{"logs", "--follow", "ctr"},
		ExitCode: 1,
		Stderr:   "container exited",
	}
	target := &terminalJoinedContextTarget{err: errors.Join(context.Canceled, terminal)}
	err := ForLog("never").
		WithStartupTimeout(300*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected terminal log stream error")
	}
	if !errors.Is(err, terminal) {
		t.Fatalf("error = %v, want terminal CLIError", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	var gotCLI *cli.CLIError
	if !errors.As(err, &gotCLI) {
		t.Fatalf("error = %v, want *cli.CLIError via As", err)
	}
}

func TestRetainedNonContextCauseOpaqueTerminalFacade(t *testing.T) {
	// Opaque Is(Canceled) facade that only exposes *cli.CLIError via As filters
	// to an empty contextStrippedCause. Terminal classification must still be
	// retained so ForLog fails fast instead of reconnecting until timeout.
	err := &opaqueTerminalFacade{}
	got := retainedNonContextCause(err)
	if got == nil {
		t.Fatal("retainedNonContextCause = nil, want opaque terminal facade")
	}
	if !isTerminalStreamError(got) {
		t.Fatalf("retained = %v, want terminal stream classification", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("retained = %v, must not match canceled", got)
	}
	var gotCLI *cli.CLIError
	if !errors.As(got, &gotCLI) {
		t.Fatalf("retained = %v, want *cli.CLIError via As", got)
	}
}

func TestForLogOpaqueTerminalFacadeFailsFast(t *testing.T) {
	target := &opaqueTerminalFacadeTarget{}
	err := ForLog("never").
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected terminal opaque facade failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want immediate terminal failure, not startup timeout", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.followCalls.Load() != 1 {
		t.Fatalf("FollowLogs calls = %d, want immediate terminal failure without reconnects", target.followCalls.Load())
	}
}

func TestForLogPermanentProbeStripsJoinedContext(t *testing.T) {
	target := &permanentProbeJoinedContextTarget{}
	err := ForLog("never").
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected permanent probe error")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
}

func TestForLogNestedPermanentFacadeFailsFast(t *testing.T) {
	target := &logNestedPermanentFacadeTarget{}
	err := ForLog("never").
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected permanent nested facade failure")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound via outer permanent facade", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.runningCalls.Load() != 1 {
		t.Fatalf("Running calls = %d, want immediate permanent failure without retries", target.runningCalls.Load())
	}
}

func TestForLogContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := &logContextOnlyPermanentTarget{}
	err := ForLog("never").
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected startup timeout after context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded after retries", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.followCalls.Load() < 2 {
		t.Fatalf("FollowLogs calls = %d, want retries for context-only permanent launch error", target.followCalls.Load())
	}
}

type permanentProbeJoinedContextTarget struct{}

func (*permanentProbeJoinedContextTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*permanentProbeJoinedContextTarget) Running(context.Context) (bool, error) {
	return false, errors.Join(context.Canceled, ErrTargetNotFound)
}
func (*permanentProbeJoinedContextTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*permanentProbeJoinedContextTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type logNestedPermanentFacadeTarget struct {
	runningCalls atomic.Int32
}

func (*logNestedPermanentFacadeTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *logNestedPermanentFacadeTarget) Running(context.Context) (bool, error) {
	t.runningCalls.Add(1)
	return false, &nestedPermanentFacade{
		err: &launchWrapper{err: context.Canceled},
	}
}
func (*logNestedPermanentFacadeTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*logNestedPermanentFacadeTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type terminalJoinedContextTarget struct {
	err error
}

func (*terminalJoinedContextTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*terminalJoinedContextTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *terminalJoinedContextTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return nil, t.err
}
func (*terminalJoinedContextTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type opaqueTerminalFacade struct{}

func (*opaqueTerminalFacade) Error() string { return "opaque terminal canceled" }
func (*opaqueTerminalFacade) Is(target error) bool {
	return target == context.Canceled
}
func (*opaqueTerminalFacade) As(target any) bool {
	if dest, ok := target.(**cli.CLIError); ok {
		*dest = &cli.CLIError{
			Args:     []string{"logs", "--follow", "ctr"},
			ExitCode: 1,
			Stderr:   "container exited",
		}
		return true
	}
	return false
}

type opaqueTerminalFacadeTarget struct {
	followCalls atomic.Int32
}

func (*opaqueTerminalFacadeTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*opaqueTerminalFacadeTarget) Running(context.Context) (bool, error) {
	return true, nil
}
func (t *opaqueTerminalFacadeTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.followCalls.Add(1)
	return nil, &opaqueTerminalFacade{}
}
func (*opaqueTerminalFacadeTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type logContextOnlyPermanentTarget struct {
	followCalls atomic.Int32
}

func (*logContextOnlyPermanentTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*logContextOnlyPermanentTarget) Running(context.Context) (bool, error) {
	return true, nil
}
func (t *logContextOnlyPermanentTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.followCalls.Add(1)
	return nil, &exec.Error{Name: "container", Err: context.Canceled}
}
func (*logContextOnlyPermanentTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
