package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
		s.calls = append(s.calls, args)
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
	ctr := &Container{id: "myctr", uid: testDockerUID, runner: runner, eng: dockerEngine{}}

	err := wait.ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err == nil {
		t.Fatal("status-less stream terminal error was hidden by readiness")
	}
	var got *CLIError
	if !errors.As(err, &got) || got != terminal {
		t.Fatalf("error = %v, want read CLIError %v", err, terminal)
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
		fakeRunner: &fakeRunner{systemUp: false},
		stream:     newTerminalStatusReader(terminal),
	}
	ctr := &Container{id: "myctr", uid: testDockerUID, runner: runner, eng: dockerEngine{}}
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
	if !errors.As(terminalErr, &got) || got != terminal {
		t.Fatalf("terminal error = %v, want original CLIError %v", terminalErr, terminal)
	}
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
	ctr := &Container{id: "myctr", uid: testDockerUID, runner: r, eng: dockerEngine{}}

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
