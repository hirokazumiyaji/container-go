package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
	cause   error
	block   bool
	started chan struct{}
	once    sync.Once
}

func newExecTarget(cause error, block bool) *execTarget {
	return &execTarget{cause: cause, block: block, started: make(chan struct{})}
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
	t.once.Do(func() { close(t.started) })
	if t.block {
		<-ctx.Done()
	}
	return 0, t.cause
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

var errCustomWaitClass = errors.New("custom wait classification")

// customCauseWrapper supplies both a virtual Is classification and As
// forwarding, like a caller-owned error wrapper.
type customCauseWrapper struct {
	cause  error
	hidden string
}

func (e *customCauseWrapper) Error() string {
	if e.hidden != "" {
		return e.hidden
	}
	return "custom wrapper: " + e.cause.Error()
}
func (e *customCauseWrapper) Unwrap() error { return e.cause }
func (e *customCauseWrapper) Is(target error) bool {
	return target == errCustomWaitClass || errors.Is(e.cause, target)
}
func (e *customCauseWrapper) As(target any) bool { return errors.As(e.cause, target) }

// sanitizingWrapper unwraps a raw cause tree but never renders it.
type sanitizingWrapper struct {
	err error
}

func (e *sanitizingWrapper) Error() string { return "operation failed" }
func (e *sanitizingWrapper) Unwrap() error { return e.err }

type contextOnlyClassWrapper struct{}

func (*contextOnlyClassWrapper) Error() string        { return "target vanished" }
func (*contextOnlyClassWrapper) Unwrap() error        { return context.Canceled }
func (*contextOnlyClassWrapper) Is(target error) bool { return target == errCustomWaitClass }

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
	if !errors.As(err, &typed) || typed != cause {
		t.Fatalf("error = %v, want typed check cause", err)
	}
	if got := errors.Unwrap(err); got != context.Canceled {
		t.Fatalf("errors.Unwrap = %v, want context.Canceled", got)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("error = %q, want retained cause text", err)
	}
}

func TestPollRetainsCheckErrorAtCallerDeadline(t *testing.T) {
	cause := &typedCause{message: "backend failed at deadline"}
	caller := newManualDeadlineContext(time.Now().Add(time.Hour))
	started := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- poll(caller, options{startupTimeout: time.Hour}, &noopTarget{}, "wait", func(checkCtx context.Context) error {
			once.Do(func() { close(started) })
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
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want deadline and cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match cancellation", err)
	}
}

func TestPollSkipsDiagnosticProbeAfterCallerTermination(t *testing.T) {
	cause := &typedCause{message: "readiness backend failure"}
	ctx, cancel := context.WithCancel(context.Background())
	target := newExecTarget(cause, true)
	done := make(chan error, 1)
	go func() {
		done <- ForExec([]string{"probe"}).WithStartupTimeout(time.Hour).
			WithPollInterval(5*time.Millisecond).WaitUntilReady(ctx, target)
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
}

func TestPollMixedContextCauseDoesNotMatchCancellation(t *testing.T) {
	cause := &typedCause{message: "backend unavailable"}
	err := poll(
		context.Background(),
		options{startupTimeout: 30 * time.Millisecond, pollInterval: 5 * time.Millisecond},
		&noopTarget{},
		"wait for test",
		func(context.Context) error { return errors.Join(context.Canceled, cause) },
	)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want backend cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match context.Canceled", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("error = %q, want backend cause text", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, must not include context cancellation text", err)
	}
}

func TestPollCustomWrapperKeepsIsAsWithoutContext(t *testing.T) {
	cause := &typedCause{message: "backend unavailable"}
	wrapped := &customCauseWrapper{cause: errors.Join(context.Canceled, cause)}
	err := poll(
		context.Background(),
		options{startupTimeout: 30 * time.Millisecond, pollInterval: 5 * time.Millisecond},
		&noopTarget{},
		"wait for test",
		func(context.Context) error { return wrapped },
	)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !errors.Is(err, errCustomWaitClass) {
		t.Fatalf("error = %v, want custom Is classification", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want retained backend cause", err)
	}
	var got *typedCause
	if !errors.As(err, &got) || got != cause {
		t.Fatalf("error = %v, want retained cause via As", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match context.Canceled", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, must not include context cancellation text", err)
	}
}

func TestPollSanitizingWrapperKeepsSafeText(t *testing.T) {
	secret := errors.New("super-secret-token")
	err := poll(
		context.Background(),
		options{startupTimeout: 25 * time.Millisecond, pollInterval: 5 * time.Millisecond},
		&noopTarget{},
		"wait for test",
		func(context.Context) error {
			return &sanitizingWrapper{err: errors.Join(context.Canceled, secret)}
		},
	)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "operation failed") {
		t.Fatalf("error = %q, want sanitizing wrapper text", err)
	}
	if strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("error = %q, leaked hidden child text", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match context.Canceled", err)
	}
}

func TestPollContextOnlyPermanentLaunchRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &exec.Error{Name: "container", Err: context.Canceled}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(120*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want startup timeout after context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled while caller is active", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("exec calls = %d, want retries", target.execCalls.Load())
	}
}

func TestPollPermanentLaunchWrappedInContextFailsFast(t *testing.T) {
	launch := &exec.Error{Name: "container", Err: errors.New("executable file not found in $PATH")}
	target := newFakeTarget()
	target.execErr = errors.Join(context.Canceled, launch)
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want permanent launch failure")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want non-context failure", err)
	}
	var got *exec.Error
	if !errors.As(err, &got) {
		t.Fatalf("error = %v (%T), want *exec.Error", err, err)
	}
	if got.Name != launch.Name {
		t.Fatalf("exec.Error Name = %q, want %q", got.Name, launch.Name)
	}
	if !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("error = %q, want launch cause text", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("exec calls = %d, want fail-fast", target.execCalls.Load())
	}
}

func TestNonContextCauseJoinsRetainedCauses(t *testing.T) {
	cause := &typedCause{message: "backend failure"}
	got := nonContextCause(fmt.Errorf("readiness check: %w", errors.Join(context.Canceled, cause)))
	if got == nil {
		t.Fatal("mixed error dropped entirely")
	}
	if !errors.Is(got, cause) {
		t.Fatalf("filtered error = %v, want cause", got)
	}
	var typed *typedCause
	if !errors.As(got, &typed) || typed != cause {
		t.Fatalf("filtered error = %v, want cause via As", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("filtered error = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("filtered error = %q, must not render context", got.Error())
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Fatalf("filtered error = %q, want cause text", got.Error())
	}
}

func TestNonContextCauseDropsContextOnlyBranches(t *testing.T) {
	cases := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("probe: %w", context.Canceled),
		errors.Join(context.Canceled, context.DeadlineExceeded),
		&exec.Error{Name: "container", Err: context.Canceled},
	}
	for _, err := range cases {
		if got := nonContextCause(err); got != nil {
			t.Fatalf("nonContextCause(%v) = %v, want nil", err, got)
		}
	}
}

func TestNonContextCauseCustomWrapperOnlyContextClassification(t *testing.T) {
	got := nonContextCause(&contextOnlyClassWrapper{})
	if got == nil {
		t.Fatal("classified wrapper dropped")
	}
	if !errors.Is(got, errCustomWaitClass) {
		t.Fatalf("filtered error = %v, want custom classification", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("filtered error = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("filtered error = %q, must not render context", got.Error())
	}
}

func TestNonContextCauseSanitizingWrapper(t *testing.T) {
	secret := errors.New("super-secret-token")
	got := nonContextCause(&sanitizingWrapper{err: errors.Join(context.Canceled, secret)})
	if got == nil {
		t.Fatal("sanitizing wrapper dropped")
	}
	if got.Error() != "operation failed" {
		t.Fatalf("filtered error = %q, want safe wrapper text", got.Error())
	}
	if strings.Contains(got.Error(), secret.Error()) {
		t.Fatalf("filtered error = %q, leaked hidden child", got.Error())
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("filtered error = %v, must drop canceled", got)
	}
}

func TestNonContextCauseRedactedFacade(t *testing.T) {
	const secret = "super-secret-token"
	cliErr := &cli.CLIError{
		Args:     []string{"logs", "--follow", "ctr"},
		ExitCode: 1,
		Stderr:   "follow failed: " + secret,
	}
	facade := cli.WithRedactor(errors.Join(context.Canceled, cliErr), cli.NewRedactor(secret))
	got := nonContextCause(facade)
	if got == nil {
		t.Fatal("redacted facade dropped")
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("filtered error = %v, must drop canceled", got)
	}
	if strings.Contains(got.Error(), secret) || strings.Contains(got.Error(), "context canceled") {
		t.Fatalf("filtered error = %q, want redacted non-context text", got.Error())
	}
	var gotCLI *cli.CLIError
	if !errors.As(got, &gotCLI) {
		t.Fatalf("filtered error = %v, want *cli.CLIError via As", got)
	}
	if strings.Contains(gotCLI.Error(), secret) {
		t.Fatalf("errors.As leaked secret: %q", gotCLI.Error())
	}
}

func TestNonContextCauseContextOnlyRedactedFacadeDropped(t *testing.T) {
	facade := cli.WithRedactor(
		&exec.Error{Name: "container", Err: context.Canceled},
		cli.NewRedactor("secret"),
	)
	if got := nonContextCause(facade); got != nil {
		t.Fatalf("context-only redacted facade = %v, want nil", got)
	}
}

func TestStructuredWaitErrorKeepsPartsSeparate(t *testing.T) {
	check := &typedCause{message: "check failed"}
	probe := &typedCause{message: "probe failed"}
	joined := nonContextCause(errors.Join(context.Canceled, check))
	err := waitTimeoutError("wait for test", 2*time.Second, joined, probe)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, check) || !errors.Is(err, probe) {
		t.Fatalf("error = %v, want deadline, check and probe causes", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	text := err.Error()
	if !strings.Contains(text, "timed out after") ||
		!strings.Contains(text, check.Error()) ||
		!strings.Contains(text, probe.Error()) {
		t.Fatalf("error = %q, want structured parts", text)
	}
	if strings.Contains(text, "context canceled") {
		t.Fatalf("error = %q, must not render context", text)
	}
}

func TestWaitFailureRedactsRetainedCause(t *testing.T) {
	const secret = "retained-secret-value"
	raw := &cli.CLIError{Args: []string{"exec", secret}, ExitCode: 1, Stderr: secret}
	err := wrapWaitCause("wait for exec", raw)
	if err == nil {
		t.Fatal("want wait failure")
	}
	err = safeDiagnosticError(err, secret)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, leaked secret", err)
	}
	var got *cli.CLIError
	if !errors.As(err, &got) {
		t.Fatalf("error = %v, want *cli.CLIError via As", err)
	}
	if got == raw || strings.Contains(got.Error(), secret) {
		t.Fatalf("errors.As returned an unsafe CLIError: %q", got.Error())
	}
}

// logStreamTarget first exposes a transient stream cause, then blocks on a
// later connection and signals that the cause has been retained.
type logStreamTarget struct {
	cause   error
	started chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (t *logStreamTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (*logStreamTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *logStreamTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	if t.calls.Add(1) == 1 {
		return &waitContractErrorReadCloser{err: t.cause}, nil
	}
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*logStreamTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

func TestForLogRetainsStreamCauseAcrossReconnect(t *testing.T) {
	cause := &typedCause{message: "scanner backend failure"}
	caller := newManualDeadlineContext(time.Now().Add(time.Hour))
	target := &logStreamTarget{cause: cause, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(caller, target)
	}()
	select {
	case <-target.started:
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
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want deadline and stream cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("error = %q, want stream cause text", err)
	}
}

func TestForLogMixedStreamCauseDropsContext(t *testing.T) {
	cause := &typedCause{message: "scanner backend failure"}
	caller := newManualDeadlineContext(time.Now().Add(time.Hour))
	target := &logStreamTarget{
		cause:   fmt.Errorf("follow logs: %w", errors.Join(context.Canceled, cause)),
		started: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- ForLog("never").WithStartupTimeout(time.Hour).WaitUntilReady(caller, target)
	}()
	select {
	case <-target.started:
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
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want deadline and stream cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, must not render context", err)
	}
}

type joinedTerminalTarget struct {
	err   error
	calls atomic.Int32
}

func (*joinedTerminalTarget) Endpoint(context.Context, string) (string, error) {
	return "", errors.New("endpoint not used")
}
func (*joinedTerminalTarget) Running(context.Context) (bool, error) { return true, nil }
func (t *joinedTerminalTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.calls.Add(1)
	return &waitContractErrorReadCloser{err: t.err}, nil
}
func (*joinedTerminalTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, errors.New("exec not used")
}

func TestForLogTerminalJoinedContextFailsFast(t *testing.T) {
	terminal := &cli.CLIError{Args: []string{"logs", "--follow", "ctr"}, ExitCode: 17, Stderr: "container exited"}
	target := &joinedTerminalTarget{err: errors.Join(context.Canceled, terminal)}
	err := ForLog("never").
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want terminal stream failure")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want non-context failure", err)
	}
	var got *cli.CLIError
	if !errors.As(err, &got) || got.ExitCode != terminal.ExitCode {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if !strings.Contains(err.Error(), "container exited") {
		t.Fatalf("error = %q, want terminal diagnostics", err)
	}
	if target.calls.Load() != 1 {
		t.Fatalf("FollowLogs calls = %d, want fail-fast", target.calls.Load())
	}
}

type finalJoinedTarget struct {
	execDone atomic.Bool
}

func (*finalJoinedTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalJoinedTarget) Running(context.Context) (bool, error) {
	if t.execDone.Load() {
		return false, errors.Join(context.Canceled, ErrTargetNotFound)
	}
	return true, nil
}
func (*finalJoinedTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalJoinedTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

func TestFinalLifecycleJoinedContextFailsFast(t *testing.T) {
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), &finalJoinedTarget{})
	if err == nil {
		t.Fatal("want final lifecycle failure")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, must not render context", err)
	}
}

type finalRetryTarget struct {
	execDone atomic.Bool
	probes   atomic.Int32
}

func (*finalRetryTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *finalRetryTarget) Running(context.Context) (bool, error) {
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
func (*finalRetryTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (t *finalRetryTarget) ExecCommand(context.Context, []string) (int, error) {
	t.execDone.Store(true)
	return 0, nil
}

func TestFinalLifecycleContextOnlyProbeRetries(t *testing.T) {
	target := &finalRetryTarget{}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady = %v, want success after retrying context-only probe", err)
	}
	if target.probes.Load() < 3 {
		t.Fatalf("lifecycle probes = %d, want transient, context-only, success", target.probes.Load())
	}
}

type markDoneTarget interface {
	markChildDone()
}

type markDoneStrategy struct{}

func (*markDoneStrategy) WaitUntilReady(_ context.Context, target Target) error {
	if marker, ok := target.(markDoneTarget); ok {
		marker.markChildDone()
	}
	return nil
}

type compositeJoinedTarget struct {
	childDone atomic.Bool
}

func (*compositeJoinedTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeJoinedTarget) Running(context.Context) (bool, error) {
	if t.childDone.Load() {
		return false, errors.Join(context.Canceled, ErrTargetNotFound)
	}
	return true, nil
}
func (*compositeJoinedTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeJoinedTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *compositeJoinedTarget) markChildDone() { t.childDone.Store(true) }

func TestCompositeFinalLifecycleJoinedContextFailsFast(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&markDoneStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&markDoneStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			err := strategy.WaitUntilReady(context.Background(), &compositeJoinedTarget{})
			if err == nil {
				t.Fatal("want final lifecycle failure")
			}
			if !errors.Is(err, ErrTargetNotFound) {
				t.Fatalf("error = %v, want ErrTargetNotFound", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, must not match canceled", err)
			}
		})
	}
}

type compositeRetryTarget struct {
	childDone atomic.Bool
	probes    atomic.Int32
}

func (*compositeRetryTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositeRetryTarget) Running(context.Context) (bool, error) {
	if !t.childDone.Load() {
		return true, nil
	}
	if t.probes.Add(1) == 1 {
		return false, context.Canceled
	}
	return true, nil
}
func (*compositeRetryTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositeRetryTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *compositeRetryTarget) markChildDone() { t.childDone.Store(true) }

func TestCompositeFinalLifecycleContextOnlyProbeRetries(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&markDoneStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&markDoneStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			target := &compositeRetryTarget{}
			if err := strategy.WaitUntilReady(context.Background(), target); err != nil {
				t.Fatalf("WaitUntilReady = %v, want success after retrying context-only probe", err)
			}
			if target.probes.Load() < 2 {
				t.Fatalf("lifecycle probes = %d, want context-only then success", target.probes.Load())
			}
		})
	}
}

type compositePermanentTarget struct {
	childDone atomic.Bool
}

func (*compositePermanentTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (t *compositePermanentTarget) Running(context.Context) (bool, error) {
	if t.childDone.Load() {
		return false, ErrContainerNotFound
	}
	return true, nil
}
func (*compositePermanentTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*compositePermanentTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *compositePermanentTarget) markChildDone() { t.childDone.Store(true) }

func TestCompositeFinalLifecyclePermanentCauseFailsFast(t *testing.T) {
	for name, strategy := range map[string]Strategy{
		"all": ForAll(&markDoneStrategy{}).WithStartupTimeout(time.Second),
		"any": ForAny(&markDoneStrategy{}).WithStartupTimeout(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := strategy.WaitUntilReady(context.Background(), &compositePermanentTarget{})
			if err == nil || !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("error = %v, want ErrContainerNotFound", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, must fail fast without the startup budget", err)
			}
			if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
				t.Fatalf("wait took %v, want fail-fast", elapsed)
			}
		})
	}
}

type contextAwareChild struct {
	cause   error
	started chan struct{}
	once    sync.Once
}

func (s *contextAwareChild) WaitUntilReady(ctx context.Context, _ Target) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return s.cause
}

func TestCompositeRetainsChildCauseBesideTermination(t *testing.T) {
	cause := &typedCause{message: "child backend failure"}
	cases := []struct {
		name    string
		want    error
		trigger func(t *testing.T, caller context.CancelFunc, started <-chan struct{})
	}{
		{
			name: "caller-cancel",
			want: context.Canceled,
			trigger: func(t *testing.T, caller context.CancelFunc, started <-chan struct{}) {
				select {
				case <-started:
					caller()
				case <-time.After(time.Second):
					t.Fatal("child did not start")
				}
			},
		},
		{
			name:    "startup-timeout",
			want:    context.DeadlineExceeded,
			trigger: func(*testing.T, context.CancelFunc, <-chan struct{}) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := &contextAwareChild{cause: cause, started: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startup := 25 * time.Millisecond
			if tc.name == "caller-cancel" {
				startup = time.Hour
			}
			strategy := ForAny(child).WithStartupTimeout(startup)
			done := make(chan error, 1)
			go func() { done <- strategy.WaitUntilReady(ctx, &noopTarget{}) }()
			tc.trigger(t, cancel, child.started)
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("ForAny did not return")
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want %v and child cause", err, tc.want)
			}
			if tc.want == context.DeadlineExceeded && errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, must not match canceled", err)
			}
		})
	}
}

func TestForExecTimeoutKeepsBackendCauseWithoutContext(t *testing.T) {
	backend := &typedCause{message: "backend probe failed"}
	target := newFakeTarget()
	target.execErr = errors.Join(context.Canceled, backend)
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, backend) {
		t.Fatalf("error = %v, want deadline and backend cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, must not render context", err)
	}
}

func TestForExecIndependentDNSFailsFast(t *testing.T) {
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
		t.Fatal("want independent DNS failure")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want immediate non-context failure", err)
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("error = %q, want independent DNS reason", err)
	}
	if errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want no sentinel classification", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("exec calls = %d, want fail-fast", target.execCalls.Load())
	}
}

type opaquePermanentFacade struct{}

func (*opaquePermanentFacade) Error() string { return "opaque permanent" }
func (*opaquePermanentFacade) Is(target error) bool {
	return target == context.Canceled || target == ErrTargetNotFound
}

func TestForExecOpaquePermanentClassificationFailsFast(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &opaquePermanentFacade{}
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(500*time.Millisecond).
		WithPollInterval(50*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want permanent opaque failure")
	}
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error = %v, want ErrTargetNotFound via opaque classification", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if target.execCalls.Load() != 1 {
		t.Fatalf("exec calls = %d, want fail-fast", target.execCalls.Load())
	}
}

func TestForExecRedactedContextOnlyPermanentRetries(t *testing.T) {
	target := newFakeTarget()
	target.execErr = cli.WithRedactor(
		&exec.Error{Name: "container", Err: context.Canceled},
		cli.NewRedactor("secret"),
	)
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(120*time.Millisecond).
		WithPollInterval(10*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want startup timeout after context-only permanent retries")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, must not match canceled", err)
	}
	if target.execCalls.Load() < 2 {
		t.Fatalf("exec calls = %d, want retries", target.execCalls.Load())
	}
}

func TestConfigErrorAsRedactedRemainsClassifiable(t *testing.T) {
	const secret = "config-secret-value"
	cfg := &ConfigError{
		Strategy: "ForLog",
		Field:    "pattern",
		Value:    secret,
		Reason:   "invalid pattern " + secret,
	}
	safe := cli.WithRedactor(cfg, cli.NewRedactor(secret))
	if !errors.Is(safe, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", safe)
	}
	var got *ConfigError
	if !errors.As(safe, &got) {
		t.Fatalf("error = %v, want *ConfigError", safe)
	}
	if got == cfg || strings.Contains(got.Error(), secret) {
		t.Fatalf("errors.As returned an unsafe ConfigError: %q", got.Error())
	}
}
