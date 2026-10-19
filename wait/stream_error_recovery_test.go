package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestPollRetriesTransientCLIInspectError(t *testing.T) {
	target := newLifecycleTarget(StateUnknown)
	target.stateErr = &cli.CLIError{Args: []string{"inspect", "myctr"}, ExitCode: 1, Stderr: "daemon temporarily unavailable"}
	start := time.Now()
	err := poll(
		context.Background(),
		options{startupTimeout: 80 * time.Millisecond, pollInterval: 10 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return errors.New("not ready") },
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want retry until timeout", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("poll returned after %v, want transient inspect retries", elapsed)
	}
}

func TestForAllFailsFastForCustomStrategyLifecycle(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}

	start := time.Now()
	err := ForAll(blockingStrategy{}).
		WithStartupTimeout(2*time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error = %v, want stopped lifecycle failure", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("ForAll took %v, want lifecycle fail-fast", elapsed)
	}
}

var errCheckCause = errors.New("readiness check failed")

type callerDeadlineTarget struct {
	stateCalls atomic.Int32
}

func (*callerDeadlineTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (t *callerDeadlineTarget) Running(ctx context.Context) (bool, error) {
	if t.stateCalls.Add(1) == 1 {
		return true, nil
	}
	<-ctx.Done()
	return false, ctx.Err()
}

func (*callerDeadlineTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (*callerDeadlineTarget) ExecCommand(context.Context, []string) (int, error) {
	return 1, errCheckCause
}

func TestFinalStateProbeHonorsCallerDeadline(t *testing.T) {
	target := &callerDeadlineTarget{}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := ForExec([]string{"probe"}).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(ctx, target)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
	if !errors.Is(err, errCheckCause) {
		t.Fatalf("error = %v, want readiness check cause", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("wait took %v, want no post-caller-deadline probe", elapsed)
	}
	if got := target.stateCalls.Load(); got != 1 {
		t.Fatalf("state calls = %d, want only the initial lifecycle probe", got)
	}
}

func TestReopenedLogsDeduplicateReplayedPrefix(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\n")),
		io.NopCloser(strings.NewReader("ready\nready\n")),
	}

	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.logCalls; got != 3 {
		t.Fatalf("FollowLogs calls = %d, want replay-only stream to be ignored", got)
	}
}

type terminalStreamTarget struct {
	calls atomic.Int32
}

func (*terminalStreamTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (*terminalStreamTarget) Running(context.Context) (bool, error) {
	return true, nil
}

func (t *terminalStreamTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.calls.Add(1)
	return terminalReader{}, nil
}

func (*terminalStreamTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type terminalReader struct{}

func (terminalReader) Read([]byte) (int, error) {
	return 0, &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 17, Stderr: "log stream failed"}
}

func (terminalReader) Close() error { return nil }

func TestPermanentLogStreamErrorFailsFast(t *testing.T) {
	target := &terminalStreamTarget{}
	err := ForLog("ready").
		WithStartupTimeout(300*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("expected terminal log stream error")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want *cli.CLIError", err)
	}
	if got := target.calls.Load(); got != 1 {
		t.Fatalf("FollowLogs calls = %d, want no retry after terminal error", got)
	}
}

type dataThenErrorTarget struct{}

func (*dataThenErrorTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*dataThenErrorTarget) Running(context.Context) (bool, error) { return true, nil }
func (*dataThenErrorTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return &dataThenErrorReader{}, nil
}
func (*dataThenErrorTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type dataThenErrorReader struct {
	sent bool
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "ready\n"), nil
	}
	return 0, &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 19, Stderr: "late stream failure"}
}

func (*dataThenErrorReader) Close() error { return nil }

func TestTerminalErrorAfterMatchingLineWins(t *testing.T) {
	err := ForLog("ready").
		WithStartupTimeout(300*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), &dataThenErrorTarget{})
	if err == nil {
		t.Fatal("matching line hid a terminal stream error")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 19 {
		t.Fatalf("error = %v, want terminal exit 19", err)
	}
}
