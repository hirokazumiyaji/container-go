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
)

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

func TestLogsWithOptionsPassesTailAndSince(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	f.calls = nil

	since := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	rc, err := ctr.LogsWithOptions(context.Background(), LogsOptions{Tail: 50, Since: since})
	if err != nil {
		t.Fatalf("LogsWithOptions: %v", err)
	}
	_ = rc.Close()

	call := f.callWith("logs")
	if call == nil {
		t.Fatal("no logs call recorded")
	}
	joined := strings.Join(call, " ")
	if !strings.Contains(joined, "--tail 50") {
		t.Errorf("missing --tail 50: %v", call)
	}
	if !strings.Contains(joined, "--since") {
		t.Errorf("missing --since: %v", call)
	}
	tailIdx := strings.Index(joined, "--tail")
	idIdx := strings.LastIndex(joined, "myctr")
	if tailIdx < 0 || idIdx < 0 || tailIdx > idIdx {
		t.Errorf("flags must precede container id: %v", call)
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
	ctr := &Container{id: "myctr", runner: r, eng: dockerEngine{}}

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
	if !strings.Contains(cliErr.Stderr, "No such container: myctr") {
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
printf 'Error response from daemon: No such container: myctr\n' >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
