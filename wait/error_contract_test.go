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

type waitContractTarget struct {
	caller                 context.Context
	started                chan struct{}
	startedOnce            sync.Once
	runningAfterCallerDone atomic.Int32
}

func newWaitContractTarget(caller context.Context) *waitContractTarget {
	return &waitContractTarget{caller: caller, started: make(chan struct{})}
}

func (t *waitContractTarget) markStarted() {
	t.startedOnce.Do(func() { close(t.started) })
}

func (t *waitContractTarget) Endpoint(ctx context.Context, _ string) (string, error) {
	t.markStarted()
	<-ctx.Done()
	return "", ctx.Err()
}

func (t *waitContractTarget) Running(_ context.Context) (bool, error) {
	if waitContractContextDone(t.caller) {
		t.runningAfterCallerDone.Add(1)
	}
	return true, nil
}

func (t *waitContractTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	t.markStarted()
	return &waitContractReadCloser{ctx: ctx, closed: make(chan struct{})}, nil
}

func (t *waitContractTarget) ExecCommand(ctx context.Context, _ []string) (int, error) {
	t.markStarted()
	<-ctx.Done()
	return 0, ctx.Err()
}

func waitContractContextDone(ctx context.Context) bool {
	if ctx == nil || ctx.Done() == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

type waitContractReadCloser struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (r *waitContractReadCloser) Read([]byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.closed:
		return 0, io.ErrClosedPipe
	}
}

func (r *waitContractReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type waitContractFactory struct {
	name string
	make func(time.Duration) Strategy
}

func waitContractFactories() []waitContractFactory {
	return []waitContractFactory{
		{
			name: "listening-port",
			make: func(timeout time.Duration) Strategy {
				return ForListeningPort("6379/tcp").
					WithStartupTimeout(timeout).
					WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "exposed-port",
			make: func(timeout time.Duration) Strategy {
				return ForExposedPort().
					WithStartupTimeout(timeout).
					WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "http",
			make: func(timeout time.Duration) Strategy {
				return ForHTTP("/health").
					WithStartupTimeout(timeout).
					WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "exec",
			make: func(timeout time.Duration) Strategy {
				return ForExec([]string{"pg_isready"}).
					WithStartupTimeout(timeout).
					WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "log",
			make: func(timeout time.Duration) Strategy {
				return ForLog("never").WithStartupTimeout(timeout)
			},
		},
		{
			name: "all",
			make: func(timeout time.Duration) Strategy {
				return ForAll(
					ForExec([]string{"pg_isready"}).
						WithStartupTimeout(time.Hour).
						WithPollInterval(5 * time.Millisecond),
				).WithStartupTimeout(timeout)
			},
		},
		{
			name: "any",
			make: func(timeout time.Duration) Strategy {
				return ForAny(
					ForExec([]string{"pg_isready"}).
						WithStartupTimeout(time.Hour).
						WithPollInterval(5 * time.Millisecond),
				).WithStartupTimeout(timeout)
			},
		},
	}
}

func TestBuiltInWaitContextErrorMatrix(t *testing.T) {
	cases := []struct {
		name       string
		strategy   time.Duration
		want       error
		callerMode string
	}{
		{name: "startup-timeout", strategy: 35 * time.Millisecond, want: context.DeadlineExceeded, callerMode: "background"},
		{name: "caller-cancel", strategy: time.Hour, want: context.Canceled, callerMode: "cancel"},
		{name: "caller-deadline", strategy: time.Hour, want: context.DeadlineExceeded, callerMode: "deadline"},
	}

	for _, factory := range waitContractFactories() {
		factory := factory
		t.Run(factory.name, func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					var ctx context.Context
					var cancel context.CancelFunc
					switch tc.callerMode {
					case "background":
						ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					case "cancel":
						ctx, cancel = context.WithCancel(context.Background())
					case "deadline":
						ctx, cancel = context.WithTimeout(context.Background(), 35*time.Millisecond)
					default:
						t.Fatalf("unknown caller mode %q", tc.callerMode)
					}
					defer cancel()

					target := newWaitContractTarget(ctx)
					done := make(chan error, 1)
					go func() {
						done <- factory.make(tc.strategy).WaitUntilReady(ctx, target)
					}()

					if tc.callerMode == "cancel" {
						select {
						case <-target.started:
							cancel()
						case err := <-done:
							t.Fatalf("wait returned before a readiness probe started: %v", err)
						case <-time.After(time.Second):
							t.Fatal("readiness probe did not start")
						}
					}

					var err error
					select {
					case err = <-done:
					case <-time.After(2 * time.Second):
						t.Fatal("wait did not honor its context")
					}
					if err == nil {
						t.Fatal("wait unexpectedly succeeded")
					}
					if !errors.Is(err, tc.want) {
						t.Fatalf("error = %v, want errors.Is(..., %v)", err, tc.want)
					}
					if errors.Is(err, context.Canceled) != (tc.want == context.Canceled) {
						t.Fatalf("error = %v, unexpected context.Canceled match", err)
					}
					// Composite strategies run a lifecycle monitor that may
					// observe Running once while noticing cancellation. ForLog
					// can likewise select its lifecycle ticker in the same
					// turn as a ready deadline and probe with an already-
					// canceled context. The budget contract is that the wait
					// still returns promptly with the caller's context error.
					if tc.callerMode != "background" &&
						factory.name != "all" && factory.name != "any" && factory.name != "log" &&
						target.runningAfterCallerDone.Load() != 0 {
						t.Fatalf("Running was called %d time(s) after caller termination", target.runningAfterCallerDone.Load())
					}
				})
			}
		})
	}
}

var errWaitContractTransient = errors.New("transient readiness failure")

type waitCauseTarget struct {
	cause   error
	started chan struct{}
	once    sync.Once
}

func (t *waitCauseTarget) markStarted() {
	t.once.Do(func() {
		if t.started != nil {
			close(t.started)
		}
	})
}

func (t *waitCauseTarget) Endpoint(context.Context, string) (string, error) {
	t.markStarted()
	return "", t.cause
}

func (t *waitCauseTarget) Running(context.Context) (bool, error) { return true, nil }

func (t *waitCauseTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return &waitContractStartedErrorReadCloser{err: t.cause, mark: t.markStarted}, nil
}

func (t *waitCauseTarget) ExecCommand(context.Context, []string) (int, error) {
	t.markStarted()
	return 0, t.cause
}

type waitContractErrorReadCloser struct {
	err error
}

func (r *waitContractErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (*waitContractErrorReadCloser) Close() error               { return nil }

type waitContractStartedErrorReadCloser struct {
	err  error
	mark func()
}

func (r *waitContractStartedErrorReadCloser) Read([]byte) (int, error) {
	if r.mark != nil {
		r.mark()
	}
	return 0, r.err
}
func (*waitContractStartedErrorReadCloser) Close() error { return nil }

func TestWaitErrorRetainsLastTransientCause(t *testing.T) {
	factories := []waitContractFactory{
		{
			name: "listening-port",
			make: func(timeout time.Duration) Strategy {
				return ForListeningPort("6379/tcp").WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "exposed-port",
			make: func(timeout time.Duration) Strategy {
				return ForExposedPort().WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "http",
			make: func(timeout time.Duration) Strategy {
				return ForHTTP("/health").WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "exec",
			make: func(timeout time.Duration) Strategy {
				return ForExec([]string{"pg_isready"}).WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond)
			},
		},
		{
			name: "all",
			make: func(timeout time.Duration) Strategy {
				return ForAll(
					ForExec([]string{"pg_isready"}).WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond),
				)
			},
		},
		{
			name: "any",
			make: func(timeout time.Duration) Strategy {
				return ForAny(
					ForExec([]string{"pg_isready"}).WithStartupTimeout(timeout).WithPollInterval(5 * time.Millisecond),
				)
			},
		},
	}

	for _, factory := range factories {
		factory := factory
		t.Run(factory.name, func(t *testing.T) {
			caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- factory.make(time.Hour).WaitUntilReady(caller, &waitCauseTarget{
					cause:   errWaitContractTransient,
					started: started,
				})
			}()
			select {
			case <-started:
				caller.expire()
			case <-time.After(time.Second):
				t.Fatal("readiness probe did not start")
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("wait did not return after deadline")
			}
			if err == nil {
				t.Fatal("wait unexpectedly succeeded")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want context.DeadlineExceeded", err)
			}
			if !errors.Is(err, errWaitContractTransient) {
				t.Fatalf("error = %v, want last transient cause", err)
			}
			if !strings.Contains(err.Error(), errWaitContractTransient.Error()) {
				t.Fatalf("error = %v, want transient cause in diagnostic text", err)
			}
		})
	}

	t.Run("log-stream", func(t *testing.T) {
		caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- ForLog("never").WithStartupTimeout(time.Hour).
				WaitUntilReady(caller, &waitCauseTarget{cause: errWaitContractTransient, started: started})
		}()
		select {
		case <-started:
			caller.expire()
		case <-time.After(time.Second):
			t.Fatal("log stream probe did not start")
		}
		var err error
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("wait did not return after deadline")
		}
		if err == nil {
			t.Fatal("wait unexpectedly succeeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if !errors.Is(err, errWaitContractTransient) {
			t.Fatalf("error = %v, want log stream cause", err)
		}
	})
}

type waitDiagnosticTarget struct {
	execStarted    chan struct{}
	runningStarted chan struct{}
	execOnce       sync.Once
	runningOnce    sync.Once
}

func newWaitDiagnosticTarget() *waitDiagnosticTarget {
	return &waitDiagnosticTarget{
		execStarted:    make(chan struct{}),
		runningStarted: make(chan struct{}),
	}
}

func (*waitDiagnosticTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("unused endpoint")
}

func (t *waitDiagnosticTarget) Running(ctx context.Context) (bool, error) {
	t.runningOnce.Do(func() { close(t.runningStarted) })
	if err := ctx.Err(); err != nil {
		return true, err
	}
	// State probes must stay cheap. A cancelled wait that still blocks here
	// for a full second would itself violate the caller budget contract.
	return true, nil
}

func (*waitDiagnosticTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (t *waitDiagnosticTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execOnce.Do(func() { close(t.execStarted) })
	return 0, errWaitContractTransient
}

func TestCallerTerminationDoesNotOutliveDiagnosticProbe(t *testing.T) {
	cases := []struct {
		name    string
		make    func() Strategy
		trigger string
		want    error
	}{
		{
			name: "exec-cancel",
			make: func() Strategy {
				return ForExec([]string{"pg_isready"}).WithStartupTimeout(time.Hour).WithPollInterval(5 * time.Millisecond)
			},
			trigger: "exec",
			want:    context.Canceled,
		},
		{
			name: "exec-deadline",
			make: func() Strategy {
				return ForExec([]string{"pg_isready"}).WithStartupTimeout(time.Hour).WithPollInterval(5 * time.Millisecond)
			},
			trigger: "exec-deadline",
			want:    context.DeadlineExceeded,
		},
		{
			name: "log-cancel",
			make: func() Strategy {
				return ForLog("never").WithStartupTimeout(time.Hour)
			},
			trigger: "running",
			want:    context.Canceled,
		},
		{
			name: "log-deadline",
			make: func() Strategy {
				return ForLog("never").WithStartupTimeout(time.Hour)
			},
			trigger: "running-deadline",
			want:    context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			target := newWaitDiagnosticTarget()
			var ctx context.Context
			var cancel context.CancelFunc
			var expire func()
			switch tc.trigger {
			case "exec-deadline", "running-deadline":
				caller := newReviewManualDeadlineContext(time.Now().Add(time.Hour))
				ctx = caller
				cancel = func() {}
				expire = caller.expire
			default:
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- tc.make().WaitUntilReady(ctx, target) }()

			switch tc.trigger {
			case "exec":
				select {
				case <-target.execStarted:
					cancel()
				case err := <-done:
					t.Fatalf("wait returned before exec probe started: %v", err)
				case <-time.After(time.Second):
					t.Fatal("exec probe did not start")
				}
			case "exec-deadline":
				select {
				case <-target.execStarted:
					expire()
				case err := <-done:
					t.Fatalf("wait returned before exec probe started: %v", err)
				case <-time.After(time.Second):
					t.Fatal("exec probe did not start")
				}
			case "running":
				select {
				case <-target.runningStarted:
					cancel()
				case err := <-done:
					t.Fatalf("wait returned before diagnostic probe started: %v", err)
				case <-time.After(time.Second):
					t.Fatal("diagnostic probe did not start")
				}
			case "running-deadline":
				select {
				case <-target.runningStarted:
					expire()
				case err := <-done:
					t.Fatalf("wait returned before diagnostic probe started: %v", err)
				case <-time.After(time.Second):
					t.Fatal("diagnostic probe did not start")
				}
			}

			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("wait did not return after caller termination")
			}
			elapsed := time.Since(start)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if elapsed > 700*time.Millisecond {
				t.Fatalf("wait took %v after caller termination; diagnostic probe outlived caller", elapsed)
			}
		})
	}
}
