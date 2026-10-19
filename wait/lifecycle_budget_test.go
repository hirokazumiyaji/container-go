package wait

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type budgetWaitCause struct{ message string }

func (e *budgetWaitCause) Error() string { return e.message }

type budgetLogProbeTarget struct {
	streamErr   error
	probeErr    error
	probeCalls  atomic.Int32
	probeAfter  atomic.Bool
	probeBudget time.Duration
}

func (*budgetLogProbeTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("unused endpoint")
}

func (t *budgetLogProbeTarget) Running(ctx context.Context) (bool, error) {
	t.probeCalls.Add(1)
	if deadline, ok := ctx.Deadline(); ok {
		t.probeBudget = time.Until(deadline)
	}
	<-ctx.Done()
	t.probeAfter.Store(true)
	return false, t.probeErr
}

func (t *budgetLogProbeTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	if t.streamErr != nil {
		return budgetErrorReader{err: t.streamErr}, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (*budgetLogProbeTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("unused exec")
}

type budgetErrorReader struct{ err error }

func (r budgetErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (budgetErrorReader) Close() error               { return nil }

func TestForLogEOFProbeHonorsCallerBudgetAndRetainsCauses(t *testing.T) {
	streamCause := &budgetWaitCause{message: "stream read failed"}
	probeCause := &budgetWaitCause{message: "state probe failed"}
	target := &budgetLogProbeTarget{streamErr: streamCause, probeErr: probeCause}
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

type budgetCloseTerminalLogStream struct {
	*strings.Reader
	done        chan struct{}
	expire      func()
	terminalErr error
}

func newBudgetCloseTerminalLogStream(
	expire func(),
	terminalErr error,
) *budgetCloseTerminalLogStream {
	done := make(chan struct{})
	close(done)
	return &budgetCloseTerminalLogStream{
		Reader:      strings.NewReader("ready\n"),
		done:        done,
		expire:      expire,
		terminalErr: terminalErr,
	}
}

func (s *budgetCloseTerminalLogStream) Close() error {
	s.expire()
	return nil
}
func (s *budgetCloseTerminalLogStream) Done() <-chan struct{}     { return s.done }
func (*budgetCloseTerminalLogStream) Drain(context.Context) error { return nil }
func (s *budgetCloseTerminalLogStream) TerminalError() error {
	return s.terminalErr
}

type budgetDrainTerminalLogStream struct {
	*strings.Reader
	done        chan struct{}
	cancel      context.CancelFunc
	terminalErr error
}

func TestSettleLogMatchRetainsTerminalErrorWhenDrainIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", "myctr"},
		ExitCode: 17,
		Stderr:   "logs stream failed",
	}
	done := make(chan struct{})
	close(done)
	stream := &budgetDrainTerminalLogStream{
		Reader:      strings.NewReader(""),
		done:        done,
		cancel:      cancel,
		terminalErr: terminal,
	}
	err := settleLogMatch(ctx, stream, bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("settle error = %v, want context.Canceled", err)
	}
	var got *cli.CLIError
	if !errors.As(err, &got) || got != terminal {
		t.Fatalf("settle error = %v, want terminal CLIError %v", err, terminal)
	}
}

func (*budgetDrainTerminalLogStream) Close() error { return nil }
func (s *budgetDrainTerminalLogStream) Done() <-chan struct{} {
	return s.done
}
func (s *budgetDrainTerminalLogStream) Drain(context.Context) error {
	s.cancel()
	return context.Canceled
}
func (s *budgetDrainTerminalLogStream) TerminalError() error {
	return s.terminalErr
}

func TestForLogRetainsSettledTerminalErrorDuringContextTermination(t *testing.T) {
	tests := map[string]struct {
		newContext func() (context.Context, func())
		want       error
	}{
		"cancellation": {
			newContext: func() (context.Context, func()) {
				return context.WithCancel(context.Background())
			},
			want: context.Canceled,
		},
		"deadline": {
			newContext: func() (context.Context, func()) {
				ctx := newBudgetDeadlineContext(time.Now().Add(time.Hour))
				return ctx, ctx.expire
			},
			want: context.DeadlineExceeded,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			terminal := &cli.CLIError{
				Binary:   "docker",
				Args:     []string{"logs", "--follow", "myctr"},
				ExitCode: 17,
				Stderr:   "logs stream failed",
			}
			ctx, expire := tc.newContext()
			stream := newBudgetCloseTerminalLogStream(expire, terminal)
			target := &stubWaitTarget{logs: []io.ReadCloser{stream}}

			err := ForLog("ready").WithStartupTimeout(time.Hour).WaitUntilReady(ctx, target)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			var got *cli.CLIError
			if !errors.As(err, &got) {
				t.Fatalf("error = %v, want settled terminal *cli.CLIError", err)
			}
			if got == terminal {
				t.Fatalf("errors.As exposed the raw CLIError: %q", got.Error())
			}
		})
	}
}

type budgetBlockingStatusStream struct {
	reader        *io.PipeReader
	done          chan struct{}
	terminalErr   error
	terminalCalls atomic.Int32
}

func newBudgetBlockingStatusStream(terminalErr error) *budgetBlockingStatusStream {
	done := make(chan struct{})
	close(done)
	return &budgetBlockingStatusStream{
		reader:      newBlockingPipeReader(),
		done:        done,
		terminalErr: terminalErr,
	}
}

func newBlockingPipeReader() *io.PipeReader {
	reader, _ := io.Pipe()
	return reader
}

func (s *budgetBlockingStatusStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}
func (s *budgetBlockingStatusStream) Close() error { return s.reader.Close() }
func (s *budgetBlockingStatusStream) Done() <-chan struct{} {
	return s.done
}
func (s *budgetBlockingStatusStream) TerminalError() error {
	s.terminalCalls.Add(1)
	return s.terminalErr
}

func TestScanLogStreamJoinsContextWithSettledTerminalExit(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", "myctr"},
		ExitCode: 17,
		Stderr:   "logs stream failed",
	}
	stream := newBudgetBlockingStatusStream(terminal)
	t.Cleanup(func() { _ = stream.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := scanLogStream(ctx, stream, func(string) int { return 0 }, 1, logReplay{})
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", result.err)
	}
	var got *cli.CLIError
	if !errors.As(result.err, &got) || got != terminal {
		t.Fatalf("scan error = %v, want settled terminal %v", result.err, terminal)
	}
	if calls := stream.terminalCalls.Load(); calls != 1 {
		t.Fatalf("TerminalError calls = %d, want one cached terminal result", calls)
	}
}

func TestPollRunsFinalLifecycleCheckBeforeSuccess(t *testing.T) {
	target := &stubWaitTarget{runningStates: []bool{false}}
	err := poll(
		context.Background(),
		options{startupTimeout: time.Second, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want final stopped lifecycle error", err)
	}
	if got := target.runningCalls.Load(); got != 1 {
		t.Fatalf("Running calls = %d, want one final lifecycle check", got)
	}
}

func TestForLogRunsFinalLifecycleCheckBeforeSuccess(t *testing.T) {
	target := &stubWaitTarget{
		runningStates: []bool{false},
		logs:          []io.ReadCloser{io.NopCloser(strings.NewReader("ready\n"))},
	}
	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want final stopped lifecycle error", err)
	}
	if got := target.runningCalls.Load(); got != 1 {
		t.Fatalf("Running calls = %d, want one final lifecycle check", got)
	}
}

func TestFinalLifecycleProbeHonorsWaitBudget(t *testing.T) {
	target := &slowRunningTarget{fakeTarget: newFakeTarget(), block: time.Hour}
	started := time.Now()
	err := poll(
		context.Background(),
		options{startupTimeout: 20 * time.Millisecond, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want bounded final probe deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("final lifecycle probe took %v, want wait budget bound", elapsed)
	}
}

type budgetContextStrategy struct {
	cause  error
	starts chan struct{}
}

func (s *budgetContextStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	select {
	case <-s.starts:
	default:
		close(s.starts)
	}
	<-ctx.Done()
	return s.cause
}

type budgetNonCooperativeStrategy struct {
	started chan struct{}
	release chan struct{}
}

func (s *budgetNonCooperativeStrategy) WaitUntilReady(context.Context, Target) error {
	close(s.started)
	<-s.release
	return nil
}

func TestForAnyCancellationDoesNotWaitForNonCooperativeChild(t *testing.T) {
	child := &budgetNonCooperativeStrategy{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { close(child.release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ForAny(child).WithStartupTimeout(time.Hour).WaitUntilReady(ctx, &stubWaitTarget{})
	}()

	select {
	case <-child.started:
	case <-time.After(time.Second):
		t.Fatal("non-cooperative child did not start")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("ForAny took %v after cancellation", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ForAny waited for a non-cooperative child indefinitely")
	}
}

func TestCompositeContextErrorsRetainChildCauses(t *testing.T) {
	cause := &budgetWaitCause{message: "child failed at deadline"}
	tests := []struct {
		name     string
		strategy Strategy
	}{
		{
			name:     "all",
			strategy: ForAll((&budgetContextStrategy{cause: cause, starts: make(chan struct{})})).WithStartupTimeout(20 * time.Millisecond),
		},
		{
			name:     "any",
			strategy: ForAny((&budgetContextStrategy{cause: cause, starts: make(chan struct{})})).WithStartupTimeout(20 * time.Millisecond),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			err := tc.strategy.WaitUntilReady(context.Background(), &stubWaitTarget{})
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
	cause := &budgetWaitCause{message: "child saw cancellation"}
	child := &budgetContextStrategy{cause: cause, starts: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-child.starts
		cancel()
	}()
	err := ForAll(child).WithStartupTimeout(time.Hour).WaitUntilReady(ctx, &stubWaitTarget{})
	if err == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want cancellation and child cause", err)
	}
}

type budgetDeadlineContext struct {
	deadline time.Time
	done     chan struct{}
}

func newBudgetDeadlineContext(deadline time.Time) *budgetDeadlineContext {
	return &budgetDeadlineContext{deadline: deadline, done: make(chan struct{})}
}
func (c *budgetDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *budgetDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *budgetDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (*budgetDeadlineContext) Value(any) any { return nil }
func (c *budgetDeadlineContext) expire()     { close(c.done) }

type budgetReleaseStrategy struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	cause    error
}

func (s *budgetReleaseStrategy) WaitUntilReady(ctx context.Context, _ Target) error {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return s.cause
}

func TestForAllDistinguishesCallerAndStartupDeadlines(t *testing.T) {
	cause := &budgetWaitCause{message: "child released after deadline"}
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
			caller := newBudgetDeadlineContext(time.Now().Add(tc.callerDeadline))
			child := &budgetReleaseStrategy{
				started:  make(chan struct{}),
				canceled: make(chan struct{}),
				release:  make(chan struct{}),
				cause:    cause,
			}
			done := make(chan error, 1)
			go func() {
				done <- ForAll(child).WithStartupTimeout(tc.startupTimeout).WaitUntilReady(caller, &stubWaitTarget{})
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
