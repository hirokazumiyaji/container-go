package wait

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestReview91ForLogDiscardsUnterminatedFinalToken(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready\npartial")),
		io.NopCloser(strings.NewReader("ready\npartial\nready\n")),
	}

	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.logCalls != 2 {
		t.Fatalf("FollowLogs calls = %d, want later terminated ready line to be observed", target.logCalls)
	}
}

func TestReview91PollDoesNotExtendDeadlineForFinalStateProbe(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.stateScript = []struct {
		state State
		err   error
	}{
		{state: StateRunning},
		{state: StateStopped},
	}

	err := poll(
		context.Background(),
		options{startupTimeout: 30 * time.Millisecond, pollInterval: 5 * time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error { return errors.New("not ready") },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want original strategy deadline", err)
	}
	if calls := target.stateCalls; calls != 1 {
		t.Fatalf("State calls = %d, want no post-deadline probe", calls)
	}
}

func TestReview91QueuedTerminalScanResultWinsContextRace(t *testing.T) {
	cliErr := &cli.CLIError{Args: []string{"logs", "--follow", "myctr"}, ExitCode: 23, Stderr: "late terminal failure"}
	results := make(chan logScanResult, 1)
	results <- logScanResult{err: cliErr}

	got, ok := scanResultOnContextDone(results)
	if !ok {
		t.Fatal("queued scanner result was discarded on context completion")
	}
	if !errors.Is(got.err, cliErr) && got.err != cliErr {
		t.Fatalf("queued error = %v, want %v", got.err, cliErr)
	}
	scannerErrors := make(chan error, 1)
	scannerErrors <- cliErr
	if gotErr, ok := scannerErrorOnContextDone(scannerErrors); !ok || gotErr != cliErr {
		t.Fatalf("settle scanner error = %v, queued = %t, want %v", gotErr, ok, cliErr)
	}

	caller, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	wait, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	joined := logContextScanError(caller, wait, "wait for test", time.Second, nil, cliErr)
	if !errors.Is(joined, context.Canceled) {
		t.Fatalf("joined error = %v, want context.Canceled", joined)
	}
	var gotCLI *cli.CLIError
	if !errors.As(joined, &gotCLI) || gotCLI.ExitCode != 23 {
		t.Fatalf("joined error = %v, want retained CLIError", joined)
	}
}
