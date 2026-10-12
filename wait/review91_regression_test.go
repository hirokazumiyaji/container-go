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

func TestReview91PollRetriesTransientCLIInspectError(t *testing.T) {
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

func TestReview91ForAllFailsFastForCustomStrategyLifecycle(t *testing.T) {
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

var errReview91CheckCause = errors.New("readiness check failed")

type review91CallerDeadlineTarget struct {
	stateCalls atomic.Int32
}

func (*review91CallerDeadlineTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (t *review91CallerDeadlineTarget) Running(ctx context.Context) (bool, error) {
	if t.stateCalls.Add(1) == 1 {
		return true, nil
	}
	<-ctx.Done()
	return false, ctx.Err()
}

func (*review91CallerDeadlineTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (*review91CallerDeadlineTarget) ExecCommand(context.Context, []string) (int, error) {
	return 1, errReview91CheckCause
}

func TestReview91FinalStateProbeHonorsCallerDeadline(t *testing.T) {
	target := &review91CallerDeadlineTarget{}
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
	if !errors.Is(err, errReview91CheckCause) {
		t.Fatalf("error = %v, want readiness check cause", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("wait took %v, want no post-caller-deadline probe", elapsed)
	}
	if got := target.stateCalls.Load(); got != 1 {
		t.Fatalf("state calls = %d, want only the initial lifecycle probe", got)
	}
}

func TestReview91ReopenedLogsDeduplicateReplayedPrefix(t *testing.T) {
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

type review91TerminalStreamTarget struct {
	calls atomic.Int32
}

func (*review91TerminalStreamTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func (*review91TerminalStreamTarget) Running(context.Context) (bool, error) {
	return true, nil
}

func (t *review91TerminalStreamTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	t.calls.Add(1)
	return review91TerminalReader{}, nil
}

func (*review91TerminalStreamTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type review91TerminalReader struct{}

func (review91TerminalReader) Read([]byte) (int, error) {
	return 0, &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 17, Stderr: "log stream failed"}
}

func (review91TerminalReader) Close() error { return nil }

func TestReview91PermanentLogStreamErrorFailsFast(t *testing.T) {
	target := &review91TerminalStreamTarget{}
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

type review91DataThenErrorTarget struct{}

func (*review91DataThenErrorTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*review91DataThenErrorTarget) Running(context.Context) (bool, error) { return true, nil }
func (*review91DataThenErrorTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return &review91DataThenErrorReader{}, nil
}
func (*review91DataThenErrorTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

type review91DataThenErrorReader struct {
	sent bool
}

func (r *review91DataThenErrorReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "ready\n"), nil
	}
	return 0, &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 19, Stderr: "late stream failure"}
}

func (*review91DataThenErrorReader) Close() error { return nil }

func TestReview91TerminalErrorAfterMatchingLineWins(t *testing.T) {
	err := ForLog("ready").
		WithStartupTimeout(300*time.Millisecond).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), &review91DataThenErrorTarget{})
	if err == nil {
		t.Fatal("matching line hid a terminal stream error")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != 19 {
		t.Fatalf("error = %v, want terminal exit 19", err)
	}
}
