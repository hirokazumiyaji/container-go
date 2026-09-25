package container

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// execRunner extends fakeRunner with canned exec results.
type execRunner struct {
	*fakeRunner
	execStdout string
	execErr    error
}

func (e *execRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "exec" {
		e.calls = append(e.calls, args)
		for i, a := range args {
			if a == "--env-file" && i+1 < len(args) {
				data, _ := os.ReadFile(args[i+1])
				e.envFiles = append(e.envFiles, string(data))
			}
		}
		stderr := []byte("stderr-part")
		var cliErr *cli.CLIError
		if errors.As(e.execErr, &cliErr) {
			stderr = []byte(cliErr.Stderr)
		}
		return []byte(e.execStdout), stderr, e.execErr
	}
	return e.fakeRunner.Run(ctx, args...)
}

func TestExecReturnsZeroExitAndOutput(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner(), execStdout: "hello\n"}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "hello\n") {
		t.Errorf("output = %q", data)
	}

	execCall := f.callWith("exec")
	i := slices.Index(execCall, "myctr")
	if i < 0 || !slices.Equal(execCall[i+1:], []string{"echo", "hello"}) {
		t.Errorf("exec args = %v", execCall)
	}
}

func TestExecReturnsCommandExitCodeWithoutError(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "process failed"},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	data, _ := io.ReadAll(out)
	if string(data) != "process failed" {
		t.Errorf("output = %q, want %q", string(data), "process failed")
	}
}

func TestExecReportsMissingContainerAsError(t *testing.T) {
	f := &execMissingRunner{
		execRunner: &execRunner{
			fakeRunner: newTestRunner(),
			execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 1, Stderr: `not found: "myctr"`},
		},
	}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err == nil {
		t.Fatal("want error for missing container")
	}
}

type execMissingRunner struct {
	*execRunner
}

func (m *execMissingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
	}
	return m.execRunner.Run(ctx, args...)
}

func TestExecAppNotFoundStderrIsResult(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execErr:    &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "record not found"},
	}
	ctr := runTestContainer(t, f)

	code, _, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec: %v, want app result", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
}

func TestExecSuccessAddsNoProbe(t *testing.T) {
	inner := &execRunner{fakeRunner: newTestRunner(), execStdout: "ok\n"}
	r := newCountingRunner(inner)
	ctr := runTestContainer(t, r)
	before := r.count()
	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := r.count() - before; got != 1 {
		t.Fatalf("exec success calls = %d, want 1", got)
	}
}

func TestExecPassesOptionsAndEnvFile(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	_, _, err := ctr.Exec(context.Background(), []string{"id"},
		WithExecUser("nobody"), WithExecWorkDir("/tmp"),
		WithExecEnv(map[string]string{"TOKEN": "xyz"}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	joined := strings.Join(f.callWith("exec"), " ")
	if !strings.Contains(joined, "--user nobody") || !strings.Contains(joined, "--workdir /tmp") {
		t.Errorf("exec args = %s", joined)
	}
	if strings.Contains(joined, "--env ") {
		t.Errorf("exec env passed via argv: %s", joined)
	}
	if len(f.envFiles) != 1 || !strings.Contains(f.envFiles[0], "TOKEN=xyz\n") {
		t.Errorf("env-file captures = %q", f.envFiles)
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	f := &execRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), nil); err == nil {
		t.Fatal("want error for empty command")
	}
}

type issue116BlockingExecRunner struct {
	*fakeRunner
	started chan struct{}
	release chan struct{}
}

func (r *issue116BlockingExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "exec" {
		return r.fakeRunner.Run(ctx, args...)
	}
	close(r.started)
	select {
	case <-ctx.Done():
		return []byte("partial stdout"), []byte("partial stderr"), ctx.Err()
	case <-r.release:
		return []byte("partial stdout"), []byte("partial stderr"), errors.New("backend released")
	}
}

type issue116DeadlineExecRunner struct {
	*fakeRunner
	deadline time.Time
	has      bool
}

func (r *issue116DeadlineExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.deadline, r.has = ctx.Deadline()
		return []byte("ok"), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type issue116ExecResult struct {
	code int
	out  io.Reader
	err  error
}

func waitForIssue116ExecStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exec stub was not started")
	}
}

func waitForIssue116ExecResult(t *testing.T, result <-chan issue116ExecResult) issue116ExecResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatal("Exec did not return")
		return issue116ExecResult{}
	}
}

func assertExecTerminationError(t *testing.T, err error) {
	t.Helper()
	var terminationErr *ExecTerminationError
	if !errors.As(err, &terminationErr) {
		t.Fatalf("error = %v, want *ExecTerminationError", err)
	}
	if !errors.Is(err, ErrExecTerminationUnsupported) {
		t.Fatalf("error = %v, want ErrExecTerminationUnsupported", err)
	}
}

func TestExecAddsDefaultDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !f.has {
		t.Fatal("Exec passed a context without a default deadline")
	}
	remaining := time.Until(f.deadline)
	if remaining <= 0 || remaining > defaultExecTimeout+time.Second {
		t.Fatalf("default deadline = %v away, want within %v", remaining, defaultExecTimeout)
	}
}

func TestExecCustomTimeoutUsesEarlierCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(5*time.Second)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if f.deadline.After(callerDeadline) {
		t.Fatalf("deadline = %v, want no later than caller deadline %v", f.deadline, callerDeadline)
	}
}

func TestExecCustomTimeoutBoundsLaterCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(20*time.Millisecond)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	remaining := time.Until(f.deadline)
	if remaining <= 0 || remaining > 200*time.Millisecond {
		t.Fatalf("custom deadline = %v away, want it to bound the later caller deadline", remaining)
	}
}

func TestExecDefaultTimeoutStopsBlockingBackend(t *testing.T) {
	original := defaultExecTimeout
	defaultExecTimeout = 20 * time.Millisecond
	defer func() { defaultExecTimeout = original }()

	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on default timeout")
	}
	data, _ := io.ReadAll(got.out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecCancellationPreservesPartialOutput(t *testing.T) {
	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(f.release)

	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	cancel()

	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on cancellation")
	}
	data, err := io.ReadAll(got.out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if got.code != 0 || !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("code/output = %d/%q, want partial output", got.code, data)
	}
}

func TestExecTimeoutPreservesPartialOutput(t *testing.T) {
	f := &issue116BlockingExecRunner{
		fakeRunner: newTestRunner(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ctr := runTestContainer(t, f)

	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	got := waitForIssue116ExecResult(t, result)
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	if got.out == nil {
		t.Fatal("Exec returned nil output on timeout")
	}
	data, _ := io.ReadAll(got.out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecInfrastructureErrorPreservesOutput(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "diagnostic stdout",
		execErr:    errors.New("backend unavailable"),
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"})
	if err == nil {
		t.Fatal("want infrastructure error")
	}
	if code != 0 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 0 and output", code, out)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "diagnostic stdout") || !strings.Contains(string(data), "stderr-part") {
		t.Fatalf("output = %q, want diagnostic output", data)
	}
}

type issue116InfraRunner struct {
	*fakeRunner
}

func (r *issue116InfraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return []byte("partial stdout"), []byte("partial stderr"), &cli.CLIError{
			Args:     args,
			ExitCode: 125,
			Stderr:   "daemon unavailable",
		}
	}
	if len(args) > 0 && args[0] == "inspect" {
		return nil, nil, errors.New("inspect failed")
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecClassifiedInfrastructureErrorPreservesCodeAndOutput(t *testing.T) {
	f := &issue116InfraRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"})
	if err == nil {
		t.Fatal("want classified infrastructure error")
	}
	if code != 125 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 125 and output", code, out)
	}
	data, _ := io.ReadAll(out)
	if !strings.Contains(string(data), "partial stdout") || !strings.Contains(string(data), "partial stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 125 {
		t.Fatalf("error = %v, want classified CLI error with exit code 125", err)
	}
}

type issue116InspectDeadlineRunner struct {
	*fakeRunner
}

func (r *issue116InspectDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return []byte("command output"), []byte("daemon unavailable"), &cli.CLIError{Args: args, ExitCode: 125, Stderr: "daemon unavailable"}
	}
	if len(args) > 0 && args[0] == "inspect" {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecInspectDeadlineDoesNotClaimRemoteTermination(t *testing.T) {
	f := &issue116InspectDeadlineRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	if code != 125 || out == nil {
		t.Fatalf("code/output = %d/%v, want status 125 and output", code, out)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want inspect deadline", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 125 {
		t.Fatalf("error = %v, want original CLIError", err)
	}
	if errors.As(err, new(*ExecTerminationError)) {
		t.Fatalf("error = %v, inspect deadline must not claim remote termination", err)
	}
}

type issue116ConcurrentExitRunner struct {
	*fakeRunner
}

func (r *issue116ConcurrentExitRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		<-ctx.Done()
		return []byte("exit stdout"), []byte("exit stderr"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: 7, Stderr: "command failed"},
			ctx.Err(),
		)
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecContextTerminationPreservesExitStatus(t *testing.T) {
	f := &issue116ConcurrentExitRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"false"}, WithExecTimeout(20*time.Millisecond))
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want code 7 and output", code, out)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want exit status 7", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	assertExecTerminationError(t, err)
}

// Windows Process.Kill terminates the local CLI with exit code 1 rather
// than the negative status used for a Unix signal.
type issue116WindowsKillExecRunner struct {
	*fakeRunner
	stderr string
}

func (r *issue116WindowsKillExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		<-ctx.Done()
		return []byte("windows stdout"), []byte("windows stderr"), errors.Join(
			&cli.CLIError{Args: args, ExitCode: 1, Stderr: r.stderr},
			ctx.Err(),
		)
	}
	return r.fakeRunner.Run(ctx, args...)
}

type issue116WindowsKillInfraRunner struct {
	*issue116WindowsKillExecRunner
}

func (r *issue116WindowsKillInfraRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		return nil, nil, errors.New("inspect failed")
	}
	return r.issue116WindowsKillExecRunner.Run(ctx, args...)
}

func TestExecWindowsKillExitCodeStillReturnsTerminationError(t *testing.T) {
	f := &issue116WindowsKillExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertIssue116WindowsKillResult(t, issue116ExecResult{code: code, out: out, err: err})
}

func TestExecWindowsKillInfrastructureStatusStillReturnsTerminationError(t *testing.T) {
	f := &issue116WindowsKillInfraRunner{issue116WindowsKillExecRunner: &issue116WindowsKillExecRunner{
		fakeRunner: newTestRunner(),
		stderr:     "daemon unavailable",
	}}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(20*time.Millisecond))
	assertIssue116WindowsKillResult(t, issue116ExecResult{code: code, out: out, err: err})
}

func assertIssue116WindowsKillResult(t *testing.T, got issue116ExecResult) {
	t.Helper()
	if got.code != 1 || got.out == nil {
		t.Fatalf("code/output = %d/%v, want code 1 and output", got.code, got.out)
	}
	var cliErr *CLIError
	if !errors.As(got.err, &cliErr) || cliErr.ExitCode != 1 {
		t.Fatalf("error = %v, want local CLI exit status 1", got.err)
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", got.err)
	}
	assertExecTerminationError(t, got.err)
	data, err := io.ReadAll(got.out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "windows stdout") || !strings.Contains(string(data), "windows stderr") {
		t.Fatalf("output = %q, want partial output", data)
	}
}

func TestExecZeroTimeoutDisablesDefaultDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)

	if _, _, err := ctr.Exec(context.Background(), []string{"true"}, WithExecTimeout(0)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if f.has {
		t.Fatal("WithExecTimeout(0) unexpectedly added a deadline")
	}
}

func TestExecZeroTimeoutKeepsCallerDeadline(t *testing.T) {
	f := &issue116DeadlineExecRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	callerDeadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	if _, _, err := ctr.Exec(ctx, []string{"true"}, WithExecTimeout(0)); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !f.has || f.deadline.After(callerDeadline) {
		t.Fatalf("deadline = %v (has=%t), want caller deadline %v", f.deadline, f.has, callerDeadline)
	}
}

type issue116PreCanceledRunner struct {
	*fakeRunner
	execCalls int
}

func (r *issue116PreCanceledRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		r.execCalls++
		return []byte("unexpected"), nil, &cli.CLIError{Args: args, ExitCode: 7, Stderr: "unexpected"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecPreCanceledContextDoesNotLaunchCLI(t *testing.T) {
	f := &issue116PreCanceledRunner{fakeRunner: newTestRunner()}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, out, err := ctr.Exec(ctx, []string{"true"})
	if code != 0 || out == nil {
		t.Fatalf("code/output = %d/%v, want zero and an output reader", code, out)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.As(err, new(*ExecTerminationError)) {
		t.Fatalf("error = %v, pre-canceled context must not claim remote termination", err)
	}
	if f.execCalls != 0 {
		t.Fatalf("exec calls = %d, want no launch", f.execCalls)
	}
}

type issue116PlainCLIErrorRaceRunner struct {
	*fakeRunner
	started chan struct{}
}

func (r *issue116PlainCLIErrorRaceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		close(r.started)
		<-ctx.Done()
		// Deliberately omit ctx.Err from the returned error. Exec must
		// normalize the effective context from the runner's context.
		return []byte("race stdout"), []byte("race stderr"), &cli.CLIError{Args: args, ExitCode: 7, Stderr: "race"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestExecNormalizesPlainCLIErrorCancellationRace(t *testing.T) {
	f := &issue116PlainCLIErrorRaceRunner{fakeRunner: newTestRunner(), started: make(chan struct{})}
	ctr := runTestContainer(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan issue116ExecResult, 1)
	go func() {
		code, out, err := ctr.Exec(ctx, []string{"true"})
		result <- issue116ExecResult{code: code, out: out, err: err}
	}()
	waitForIssue116ExecStart(t, f.started)
	cancel()
	got := waitForIssue116ExecResult(t, result)

	if got.code != 7 || got.out == nil {
		t.Fatalf("code/output = %d/%v, want status 7 and output", got.code, got.out)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want normalized context.Canceled", got.err)
	}
	var cliErr *CLIError
	if !errors.As(got.err, &cliErr) || cliErr.ExitCode != 7 {
		t.Fatalf("error = %v, want original CLIError status 7", got.err)
	}
	assertExecTerminationError(t, got.err)
}

type issue116TimeoutError struct{}

func (issue116TimeoutError) Error() string   { return "structured operation timeout" }
func (issue116TimeoutError) Timeout() bool   { return true }
func (issue116TimeoutError) Temporary() bool { return true }

func TestExecRecognizesTimeoutAndSignalBeforeApplicationResult(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{
			name: "i/o timeout",
			err:  &cli.CLIError{Args: []string{"exec"}, ExitCode: 7, Stderr: "client: i/o timeout", OperationTimeout: true},
			code: 7,
		},
		{
			name: "command timed out",
			err:  &cli.CLIError{Args: []string{"exec"}, ExitCode: 8, Stderr: "command timed out", OperationTimeout: true},
			code: 8,
		},
		{
			name: "operation timed out",
			err:  &cli.CLIError{Args: []string{"exec"}, ExitCode: 9, Stderr: "operation timed out", OperationTimeout: true},
			code: 9,
		},
		{
			name: "structured timeout",
			err:  errors.Join(&cli.CLIError{Args: []string{"exec"}, ExitCode: 10}, issue116TimeoutError{}),
			code: 10,
		},
		{
			name: "negative signal",
			err:  &cli.CLIError{Args: []string{"exec"}, ExitCode: -1, Stderr: "signal"},
			code: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execRunner{fakeRunner: newTestRunner(), execStdout: "partial stdout", execErr: tc.err}
			ctr := runTestContainer(t, f)
			code, out, err := ctr.Exec(context.Background(), []string{"true"})
			if err == nil {
				t.Fatal("Exec returned nil error for an operation timeout/signal")
			}
			if code != tc.code || out == nil {
				t.Fatalf("code/output = %d/%v, want code %d and output", code, out, tc.code)
			}
			var cliErr *CLIError
			if !errors.As(err, &cliErr) || cliErr.ExitCode != tc.code {
				t.Fatalf("error = %v, want CLIError status %d", err, tc.code)
			}
			if f.callWith("inspect") != nil {
				t.Fatalf("timeout/signal triggered a verification probe: %v", f.calls)
			}
		})
	}
}

func TestExecDoesNotClassifyTimeoutTextFromStderr(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "application output",
		execErr: &cli.CLIError{
			Args:     []string{"exec", "myctr"},
			ExitCode: 7,
			Stderr:   "application failed: i/o timeout",
		},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec returned infrastructure error for application stderr: %v", err)
	}
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want application status 7 and output", code, out)
	}
	if f.callWith("inspect") != nil {
		t.Fatalf("application stderr triggered a verification probe: %v", f.calls)
	}
}

func TestExecDoesNotClassifyTimeoutTextFromArgs(t *testing.T) {
	f := &execRunner{
		fakeRunner: newTestRunner(),
		execStdout: "application output",
		execErr: &cli.CLIError{
			Args:     []string{"exec", "myctr", "command timed out"},
			ExitCode: 7,
			Stderr:   "application failed",
		},
	}
	ctr := runTestContainer(t, f)

	code, out, err := ctr.Exec(context.Background(), []string{"query"})
	if err != nil {
		t.Fatalf("Exec returned infrastructure error for application argv: %v", err)
	}
	if code != 7 || out == nil {
		t.Fatalf("code/output = %d/%v, want application status 7 and output", code, out)
	}
	if f.callWith("inspect") != nil {
		t.Fatalf("application argv triggered a verification probe: %v", f.calls)
	}
}
