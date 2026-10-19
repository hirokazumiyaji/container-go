package wait

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type realStreamTarget struct {
	binary string
}

func (*realStreamTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*realStreamTarget) Running(context.Context) (bool, error) { return true, nil }
func (*realStreamTarget) State(context.Context) (State, error)  { return StateRunning, nil }
func (t *realStreamTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	return (&cli.ExecRunner{Binary: t.binary}).Stream(ctx)
}
func (*realStreamTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}

func TestForLogSucceedsAfterRealStreamSettleClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stream fixture is unavailable on Windows")
	}
	binary := filepath.Join(t.TempDir(), "log-stream")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'ready\\n'\nexec sleep 10\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), &realStreamTarget{binary: binary}); err != nil {
		t.Fatalf("ForLog with a real stream: %v", err)
	}
}

func TestForLogMatchesCleanUnterminatedFinalLine(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{io.NopCloser(strings.NewReader("ready"))}

	if err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("clean final log line was not matched: %v", err)
	}
}

type trackedReadCloser struct {
	io.ReadCloser
	closed chan struct{}
	once   atomic.Bool
}

func (r *trackedReadCloser) Close() error {
	if r.once.CompareAndSwap(false, true) {
		close(r.closed)
	}
	return r.ReadCloser.Close()
}

func TestForLogSettlesBeforeAcceptingBackpressuredMatch(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	tracked := &trackedReadCloser{ReadCloser: pr, closed: make(chan struct{})}
	go func() {
		_, _ = pw.Write([]byte("ready\n"))
		for {
			if _, err := pw.Write([]byte("still running\n")); err != nil {
				return
			}
		}
	}()
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{tracked}

	if err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	select {
	case <-tracked.closed:
	default:
		t.Fatal("match was accepted before the stream was settled and closed")
	}
}

func TestForLogDiscardsUnterminatedFinalToken(t *testing.T) {
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

func TestPollDoesNotExtendDeadlineForFinalStateProbe(t *testing.T) {
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

type slowInitialStateTarget struct {
	calls   atomic.Int32
	started chan struct{}
}

func (*slowInitialStateTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (*slowInitialStateTarget) Running(context.Context) (bool, error) { return true, nil }
func (*slowInitialStateTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (*slowInitialStateTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (t *slowInitialStateTarget) State(ctx context.Context) (State, error) {
	if t.calls.Add(1) == 1 {
		close(t.started)
		<-ctx.Done()
		return StateUnknown, ctx.Err()
	}
	return StateRunning, nil
}

type waitForStateStartStrategy struct {
	target *slowInitialStateTarget
}

func (s waitForStateStartStrategy) WaitUntilReady(context.Context, Target) error {
	<-s.target.started
	return nil
}

func TestCompositeStartsChildrenWhileInitialStateProbeIsSlow(t *testing.T) {
	target := &slowInitialStateTarget{started: make(chan struct{})}
	strategy := waitForStateStartStrategy{target: target}
	err := ForAll(strategy).WithStartupTimeout(200*time.Millisecond).WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("ForAll: %v", err)
	}
}

func TestFinalSuccessRequiresRunning(t *testing.T) {
	for _, state := range []State{StateCreated, StateRestarting, StateUnknown} {
		t.Run(string(state), func(t *testing.T) {
			target := newLifecycleTarget(StateRunning)
			target.logStreams = []io.ReadCloser{io.NopCloser(strings.NewReader("ready\n"))}
			target.stateScript = []struct {
				state State
				err   error
			}{
				{state: state},
			}
			err := ForLog("ready").
				WithStartupTimeout(time.Second).
				WithPollInterval(time.Millisecond).
				WaitUntilReady(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), string(state)) {
				t.Fatalf("error = %v, state calls = %d, log calls = %d, want final %s failure", err, target.stateCalls, target.logCalls, state)
			}
		})
	}
}
