package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

const testDockerUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// streamRunner adds a canned Stream implementation to fakeRunner.
type streamRunner struct {
	*fakeRunner
	streamArgs []string
	streamData string
	closed     bool
}

type recordingCloser struct {
	io.Reader
	closed *bool
}

func (r *recordingCloser) Close() error {
	*r.closed = true
	return nil
}

func (s *streamRunner) Stream(_ context.Context, args ...string) (io.ReadCloser, error) {
	s.streamArgs = args
	return &recordingCloser{Reader: strings.NewReader(s.streamData), closed: &s.closed}, nil
}

type singleStreamRunner struct {
	*fakeRunner
	stream io.ReadCloser
}

func (r *singleStreamRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	return r.stream, nil
}

type stagedTerminalReader struct {
	terminalErr error
	stage       int
}

func (r *stagedTerminalReader) Read(p []byte) (int, error) {
	if r.stage > 0 {
		return 0, r.terminalErr
	}
	r.stage++
	return copy(p, "ready\n"), nil
}

func (*stagedTerminalReader) Close() error { return nil }

type terminalStatusReader struct {
	io.Reader
	done        chan struct{}
	terminalErr error
}

func newTerminalStatusReader(terminalErr error) *terminalStatusReader {
	done := make(chan struct{})
	close(done)
	return &terminalStatusReader{
		Reader:      strings.NewReader(""),
		done:        done,
		terminalErr: terminalErr,
	}
}

func (*terminalStatusReader) Close() error            { return nil }
func (r *terminalStatusReader) Done() <-chan struct{} { return r.done }
func (r *terminalStatusReader) TerminalError() error  { return r.terminalErr }

func TestLogsReturnsSnapshot(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	// Snapshot logs come back on stdout of a plain `logs` call.
	logsRunner := &stdoutRunner{fakeRunner: f, stdout: "log line 1\nlog line 2\n"}
	ctr.runner = logsRunner

	rc, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "log line 1\nlog line 2\n" {
		t.Errorf("logs = %q", data)
	}
	if call := f.callWith("logs"); slices.Contains(call, "--follow") {
		t.Errorf("snapshot Logs must not follow: %v", call)
	}
}

type stdoutRunner struct {
	*fakeRunner
	stdout string
}

func (s *stdoutRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "logs" {
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.mu.Unlock()
		return []byte(s.stdout), nil, nil
	}
	return s.fakeRunner.Run(ctx, args...)
}

func TestFollowLogsStreamsAndPropagatesClose(t *testing.T) {
	f := &streamRunner{fakeRunner: newTestRunner(), streamData: "streamed\n"}
	ctr := runTestContainer(t, f)

	rc, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if !slices.Equal(f.streamArgs, []string{"logs", "--follow", "myctr"}) {
		t.Errorf("stream args = %v", f.streamArgs)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "streamed\n" {
		t.Errorf("stream = %q", data)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.closed {
		t.Error("Close not propagated to the underlying stream")
	}
}

func TestFollowLogsRequiresStreamingRunner(t *testing.T) {
	f := newTestRunner() // no Stream method
	ctr := runTestContainer(t, f)

	if _, err := ctr.FollowLogs(context.Background()); err == nil {
		t.Fatal("want error when runner cannot stream")
	}
}

func TestForLogUsesReadErrorFromStatuslessStream(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", testDockerUID},
		ExitCode: 17,
		Stderr:   "logs stream failed",
	}
	runner := &singleStreamRunner{
		fakeRunner: newTestRunner(),
		stream:     &stagedTerminalReader{terminalErr: terminal},
	}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: runner, eng: dockerEngine{}}

	err := wait.ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err == nil {
		t.Fatal("status-less stream terminal error was hidden by readiness")
	}
	var got *CLIError
	if !errors.As(err, &got) || got == terminal {
		t.Fatalf("error = %v, want a redacted clone of read CLIError %v", err, terminal)
	}
}

func TestFollowLogsClassificationProbeFailurePreservesTerminalNotFound(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", testDockerUID},
		ExitCode: 1,
		Stderr:   "Error response from daemon: No such container: " + testDockerUID,
	}
	runner := &singleStreamRunner{
		fakeRunner: &fakeRunner{systemUp: false, binary: "docker"},
		stream:     newTerminalStatusReader(terminal),
	}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: runner, eng: dockerEngine{}}
	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()

	status, ok := stream.(interface{ TerminalError() error })
	if !ok {
		t.Fatalf("stream type %T does not expose terminal status", stream)
	}
	terminalErr := status.TerminalError()
	if !errors.Is(terminalErr, ErrContainerNotFound) {
		t.Fatalf("terminal error = %v, want ErrContainerNotFound", terminalErr)
	}
	if !errors.Is(terminalErr, cli.ErrSystemNotRunning) {
		t.Fatalf("terminal error = %v, want failed classification probe", terminalErr)
	}
	var got *CLIError
	if !errors.As(terminalErr, &got) {
		t.Fatalf("terminal error = %v, want a CLIError through the redaction boundary", terminalErr)
	}
	// The terminal failure crosses the handle's redaction boundary, so
	// errors.As yields a clone rather than the raw CLIError while the
	// settled exit code and stderr remain inspectable.
	if got == terminal {
		t.Fatalf("terminal error = %v, want a redacted CLIError clone", terminalErr)
	}
	if got.ExitCode != terminal.ExitCode || got.Stderr != terminal.Stderr {
		t.Fatalf("terminal CLIError = %#v, want exit code and stderr of %#v", got, terminal)
	}
}

// assertRedactedCLIError requires err to hide secret from its rendering and
// from the CLIError clone returned through the redaction boundary.
func assertRedactedCLIError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, leaks %q", err, secret)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("errors.As did not find *CLIError: %v", err)
	}
	if strings.Contains(cliErr.Stderr, secret) ||
		strings.Contains(strings.Join(cliErr.Args, " "), secret) ||
		strings.Contains(cliErr.Error(), secret) {
		t.Fatalf("redacted CLIError = %#v, leaks %q", cliErr, secret)
	}
}

func TestLogsRedactsHandleSecretsFromFailures(t *testing.T) {
	const secret = "logs-label-secret-329"
	calls := map[string]func(*Container) error{
		"Logs": func(ctr *Container) error {
			rc, err := ctr.Logs(context.Background())
			if rc != nil {
				_ = rc.Close()
			}
			return err
		},
		"LogsWithOptions": func(ctr *Container) error {
			rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{MaxBytes: 8})
			if rc != nil {
				_ = rc.Close()
			}
			return err
		},
		"LogsTo": func(ctr *Container) error {
			_, err := ctr.LogsTo(context.Background(), io.Discard, LogsOptions{MaxBytes: 8})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			f := newTestRunner()
			f.failPrefix = "logs"
			f.failStderr = "logs request failed for token " + secret
			ctr := runTestContainer(t, f, WithLabels(map[string]string{"com.example.token": secret}))

			assertRedactedCLIError(t, call(ctr), secret)
		})
	}
}

func TestFollowLogsRedactsHandleSecretsFromFailures(t *testing.T) {
	const secret = "follow-logs-label-secret-329"
	terminal := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"logs", "--follow", "myctr"},
		ExitCode: 1,
		Stderr:   "follow failed for token " + secret,
	}
	newHandle := func(t *testing.T, stream io.ReadCloser) *Container {
		t.Helper()
		runner := &singleStreamRunner{fakeRunner: newTestRunner(), stream: stream}
		return runTestContainer(t, runner, WithLabels(map[string]string{"com.example.token": secret}))
	}

	t.Run("startup", func(t *testing.T) {
		runner := &notFoundStreamRunner{fakeRunner: newTestRunner(), err: terminal}
		ctr := runTestContainer(t, runner, WithLabels(map[string]string{"com.example.token": secret}))
		_, err := ctr.FollowLogs(context.Background())
		assertRedactedCLIError(t, err, secret)
	})

	t.Run("FollowLogsTo startup", func(t *testing.T) {
		runner := &notFoundStreamRunner{fakeRunner: newTestRunner(), err: terminal}
		ctr := runTestContainer(t, runner, WithLabels(map[string]string{"com.example.token": secret}))
		assertRedactedCLIError(t, ctr.FollowLogsTo(context.Background(), io.Discard), secret)
	})

	t.Run("terminal status", func(t *testing.T) {
		ctr := newHandle(t, newTerminalStatusReader(terminal))
		stream, err := ctr.FollowLogs(context.Background())
		if err != nil {
			t.Fatalf("FollowLogs: %v", err)
		}
		defer stream.Close()
		status, ok := stream.(interface{ TerminalError() error })
		if !ok {
			t.Fatalf("stream type %T does not expose terminal status", stream)
		}
		assertRedactedCLIError(t, status.TerminalError(), secret)
	})

	t.Run("terminal status after Terminate", func(t *testing.T) {
		ctr := newHandle(t, newTerminalStatusReader(terminal))
		stream, err := ctr.FollowLogs(context.Background())
		if err != nil {
			t.Fatalf("FollowLogs: %v", err)
		}
		defer stream.Close()
		// Terminate drops the handle's diagnostic matcher. A stream the
		// caller still holds must keep the redaction it was created with.
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		status, ok := stream.(interface{ TerminalError() error })
		if !ok {
			t.Fatalf("stream type %T does not expose terminal status", stream)
		}
		assertRedactedCLIError(t, status.TerminalError(), secret)
	})

	t.Run("terminal read", func(t *testing.T) {
		ctr := newHandle(t, &stagedTerminalReader{terminalErr: terminal})
		stream, err := ctr.FollowLogs(context.Background())
		if err != nil {
			t.Fatalf("FollowLogs: %v", err)
		}
		defer stream.Close()
		_, readErr := io.ReadAll(stream)
		assertRedactedCLIError(t, readErr, secret)
	})
}

func TestLogsWithOptionsBackendMatrix(t *testing.T) {
	since := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	sinceArg := since.Format(time.RFC3339)
	const testDockerUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const testCreation = "bbbbbbbbbbbbbbbb"
	tests := []struct {
		name            string
		eng             engine
		opts            LogsOptions
		wantArgs        []string
		wantUnsupported bool
	}{
		{name: "apple tail only", eng: appleEngine{}, opts: LogsOptions{Tail: 50}, wantArgs: []string{"logs", "-n", "50", "myctr"}},
		{name: "apple since only", eng: appleEngine{}, opts: LogsOptions{Since: since}, wantUnsupported: true},
		{name: "apple tail and since", eng: appleEngine{}, opts: LogsOptions{Tail: 50, Since: since}, wantUnsupported: true},
		{name: "apple neither", eng: appleEngine{}, wantArgs: []string{"logs", "myctr"}},
		{name: "docker tail only", eng: dockerEngine{}, opts: LogsOptions{Tail: 50}, wantArgs: []string{"logs", "--tail", "50", testDockerUID}},
		{name: "docker since only", eng: dockerEngine{}, opts: LogsOptions{Since: since}, wantArgs: []string{"logs", "--since", sinceArg, testDockerUID}},
		{name: "docker tail and since", eng: dockerEngine{}, opts: LogsOptions{Tail: 50, Since: since}, wantArgs: []string{"logs", "--tail", "50", "--since", sinceArg, testDockerUID}},
		{name: "docker neither", eng: dockerEngine{}, wantArgs: []string{"logs", testDockerUID}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := &Container{id: "myctr", creation: testCreation, runner: f, eng: tc.eng}
			if _, ok := tc.eng.(dockerEngine); ok {
				ctr.setImmutableID(testDockerUID)
			}

			rc, err := ctr.LogsWithOptions(context.Background(), tc.opts)
			if tc.wantUnsupported {
				if !errors.Is(err, ErrUnsupportedCapability) {
					t.Fatalf("LogsWithOptions error = %v, want ErrUnsupportedCapability", err)
				}
				if call := f.callWith("logs"); call != nil {
					t.Errorf("unsupported options must not invoke the CLI: %v", call)
				}
				return
			}
			if err != nil {
				t.Fatalf("LogsWithOptions: %v", err)
			}
			_ = rc.Close()

			if call := f.callWith("logs"); !slices.Equal(call, tc.wantArgs) {
				t.Errorf("logs args = %v, want %v", call, tc.wantArgs)
			}
		})
	}
}

// TestDockerLogsSnapshotSincePreservesSubsecondPrecision fixes the exact
// snapshot --since argument for the public snapshot paths. Docker accepts
// RFC3339Nano, so truncating to whole seconds made an incremental caller
// replay entries already delivered from the same second.
func TestDockerLogsSnapshotSincePreservesSubsecondPrecision(t *testing.T) {
	since := time.Date(2026, 1, 2, 15, 4, 5, 123456789, time.FixedZone("JST", 9*60*60))
	wantArgs := []string{"logs", "--since", "2026-01-02T06:04:05.123456789Z", testDockerUID}
	newDockerHandle := func(f *fakeRunner) *Container {
		ctr := &Container{id: "myctr", runner: f, eng: dockerEngine{}}
		ctr.setImmutableID(testDockerUID)
		return ctr
	}

	t.Run("LogsWithOptions", func(t *testing.T) {
		f := newDockerTestRunner()
		rc, err := newDockerHandle(f).LogsWithOptions(context.Background(), LogsOptions{Since: since})
		if err != nil {
			t.Fatalf("LogsWithOptions: %v", err)
		}
		_ = rc.Close()
		if call := f.callWith("logs"); !slices.Equal(call, wantArgs) {
			t.Errorf("logs args = %v, want %v", call, wantArgs)
		}
	})

	// LogsTo shares logsArgsWithOptions, so it must emit the same timestamp.
	t.Run("LogsTo", func(t *testing.T) {
		f := newDockerTestRunner()
		if _, err := newDockerHandle(f).LogsTo(context.Background(), io.Discard, LogsOptions{Since: since}); err != nil {
			t.Fatalf("LogsTo: %v", err)
		}
		if call := f.callWith("logs"); !slices.Equal(call, wantArgs) {
			t.Errorf("logs args = %v, want %v", call, wantArgs)
		}
	})
}

func TestLogsDefaultsToUnbounded(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil
	rc, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = rc.Close()
	joined := strings.Join(f.callWith("logs"), " ")
	if strings.Contains(joined, "--tail") || strings.Contains(joined, "--since") {
		t.Errorf("default Logs must not bound: %v", joined)
	}
}

func TestFollowLogsClassifiesTerminalNotFound(t *testing.T) {
	r := &cli.ExecRunner{Binary: writeFollowLogsStub(t)}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: r, eng: dockerEngine{}}

	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()

	_, readErr := io.ReadAll(stream)
	if !errors.Is(readErr, ErrContainerNotFound) {
		t.Fatalf("read error = %v, want ErrContainerNotFound", readErr)
	}
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
	if cliErr.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "No such container: "+testDockerUID) {
		t.Errorf("Stderr = %q, want not-found diagnostic", cliErr.Stderr)
	}
}

func TestForLogPreservesFollowLogsTerminalNotFound(t *testing.T) {
	r := &cli.ExecRunner{Binary: writeFollowLogsNotFoundStub(t)}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: r, eng: dockerEngine{}}

	err := wait.ForLog("No such container: "+testDockerUID).
		WithStartupTimeout(3*time.Second).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want ErrContainerNotFound", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
	if cliErr.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "Error response from daemon") {
		t.Fatalf("stderr = %q, want terminal diagnostic", cliErr.Stderr)
	}
}

func TestForLogRejectsMatchBeforeTerminalStderrError(t *testing.T) {
	r := &cli.ExecRunner{Binary: writeFollowLogsLateErrorStub(t)}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: r, eng: dockerEngine{}}

	err := wait.ForLog("ready").
		WithStartupTimeout(3*time.Second).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("error = %v, want terminal not-found error", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *CLIError", err)
	}
}

func TestFollowLogsTerminalErrorSurvivesFailedClassificationProbe(t *testing.T) {
	r := &cli.ExecRunner{Binary: writeFollowLogsProbeFailureStub(t)}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: r, eng: dockerEngine{}}

	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()
	_, readErr := io.ReadAll(stream)
	if !errors.Is(readErr, ErrContainerNotFound) {
		t.Fatalf("read error = %v, want ErrContainerNotFound", readErr)
	}
	var cliErr *CLIError
	if !errors.As(readErr, &cliErr) {
		t.Fatalf("read error = %v, want *CLIError", readErr)
	}
}

func writeFollowLogsStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("follow-log shell stub requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
head -c 70000 /dev/zero >&2
printf 'Error response from daemon: No such container: %s\n' "$3" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFollowLogsNotFoundStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("follow-log shell stub requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'Error response from daemon: No such container: %s\n' "$3" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFollowLogsProbeFailureStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("follow-log shell stub requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  exit 1
fi
printf 'Error response from daemon: No such container: %s\n' "$3" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFollowLogsLateErrorStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("follow-log shell stub requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  printf '29.7\n'
  exit 0
fi
printf 'ready\n'
printf 'Error response from daemon: No such container: %s\n' "$3" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// lateTerminalReader exposes TerminalError without Done, so Read cannot use
// it as a settled process status: the early nil observation and the later
// failure both come from the same stream, like a process that has not exited.
type lateTerminalReader struct {
	io.Reader
	mu  sync.Mutex
	err error
}

func (*lateTerminalReader) Close() error { return nil }

func (r *lateTerminalReader) TerminalError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *lateTerminalReader) setTerminalError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func TestClassifyingStreamTerminalErrorDoesNotLatchNil(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", testDockerUID},
		ExitCode: 1,
		Stderr:   "logs stream failed",
	}
	underlying := &lateTerminalReader{Reader: strings.NewReader("")}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: &fakeRunner{systemUp: false, binary: "docker"}, eng: dockerEngine{}}
	stream := &classifyingStream{ReadCloser: underlying, ctx: context.Background(), container: ctr}

	if err := stream.TerminalError(); err != nil {
		t.Fatalf("TerminalError before exit = %v, want nil", err)
	}
	underlying.setTerminalError(terminal)
	var got *CLIError
	if err := stream.TerminalError(); !errors.As(err, &got) || got.ExitCode != terminal.ExitCode {
		t.Fatalf("TerminalError after exit = %v, want the settled CLIError", err)
	}
}

func TestClassifyingStreamReadKeepsErrorAfterEarlyNilTerminal(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", testDockerUID},
		ExitCode: 1,
		Stderr:   "logs stream failed",
	}
	underlying := &lateTerminalReader{Reader: &stagedTerminalReader{terminalErr: terminal}}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: &fakeRunner{systemUp: false, binary: "docker"}, eng: dockerEngine{}}
	stream := &classifyingStream{ReadCloser: underlying, ctx: context.Background(), container: ctr}

	if err := stream.TerminalError(); err != nil {
		t.Fatalf("TerminalError before exit = %v, want nil", err)
	}
	if _, err := stream.Read(make([]byte, 16)); err != nil {
		t.Fatalf("first Read = %v, want the available output", err)
	}
	_, err := stream.Read(make([]byte, 16))
	var got *CLIError
	if !errors.As(err, &got) || got.ExitCode != terminal.ExitCode {
		t.Fatalf("terminal Read error = %v, want a non-nil CLIError", err)
	}
}

// TestClassifyingStreamTerminalErrorRaceFreeAcrossRead is the regression for
// the unlocked terminalErr read. Readers sample TerminalError while the first
// terminal Read classifies and caches the error; without terminalMu, -race
// reports an unsynchronized read/write.
func TestClassifyingStreamTerminalErrorRaceFreeAcrossRead(t *testing.T) {
	terminal := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"logs", "--follow", testDockerUID},
		ExitCode: 1,
		Stderr:   "logs stream failed",
	}
	underlying := &lateTerminalReader{Reader: &stagedTerminalReader{terminalErr: terminal}}
	ctr := &Container{id: "myctr", sync: &lockPack{uid: testDockerUID}, runner: &fakeRunner{systemUp: false, binary: "docker"}, eng: dockerEngine{}}
	stream := &classifyingStream{ReadCloser: underlying, ctx: context.Background(), container: ctr}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				_ = stream.TerminalError()
			}
		}()
	}
	close(start)
	_, _ = stream.Read(make([]byte, 16))
	_, _ = stream.Read(make([]byte, 16))
	wg.Wait()

	if err := stream.TerminalError(); err == nil {
		t.Fatal("terminal error was not classified")
	}
}

func TestLogsToRejectsNilWriter(t *testing.T) {
	ctr := runTestContainer(t, newTestRunner())
	for _, tc := range []struct {
		name   string
		output io.Writer
	}{
		{name: "nil", output: nil},
		{name: "typed nil", output: (*bytes.Buffer)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ctr.LogsTo(context.Background(), tc.output, LogsOptions{})
			assertPublicValidationError(t, err, "LogsTo", "LogsTo", tc.output,
				"output writer cannot be nil")
		})
	}
}
