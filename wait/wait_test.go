package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTarget implements Target for tests.
type fakeTarget struct {
	endpoint   string
	state      atomic.Value
	stateErr   error
	stateCalls atomic.Int32
	logs       io.ReadCloser
	execCode   int
	execErr    error
	execCalls  atomic.Int32
}

func newFakeTarget() *fakeTarget {
	t := &fakeTarget{logs: io.NopCloser(strings.NewReader(""))}
	t.state.Store(StateRunning)
	return t
}

func (f *fakeTarget) Endpoint(_ context.Context, port string) (string, error) {
	if f.endpoint == "" {
		return "", errors.New("no endpoint configured")
	}
	return f.endpoint, nil
}

func (f *fakeTarget) Running(context.Context) (bool, error) {
	state, err := f.State(context.Background())
	return state == StateRunning, err
}

func (f *fakeTarget) State(_ context.Context) (State, error) {
	f.stateCalls.Add(1)
	return f.state.Load().(State), f.stateErr
}

type firstStateTarget struct {
	*fakeTarget
	first    State
	firstErr error
}

func (t *firstStateTarget) State(_ context.Context) (State, error) {
	t.stateCalls.Add(1)
	if t.stateCalls.Load() == 1 {
		t.state.Store(t.first)
		return t.first, t.firstErr
	}
	t.state.Store(StateRunning)
	return StateRunning, nil
}

func (f *fakeTarget) FollowLogs(_ context.Context) (io.ReadCloser, error) { return f.logs, nil }

func (f *fakeTarget) ExecCommand(_ context.Context, cmd []string) (int, error) {
	f.execCalls.Add(1)
	return f.execCode, f.execErr
}

func TestForListeningPortSucceedsWhenPortOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	target := newFakeTarget()
	target.endpoint = ln.Addr().String()

	s := ForListeningPort("6379/tcp").WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForListeningPortTimesOut(t *testing.T) {
	// A listener that is immediately closed leaves a port nobody
	// answers on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	target := newFakeTarget()
	target.endpoint = addr

	s := ForListeningPort("6379/tcp").WithStartupTimeout(300 * time.Millisecond).WithPollInterval(50 * time.Millisecond)
	err = s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want timeout error")
	}
}

func TestForListeningPortFailsFastWhenContainerStops(t *testing.T) {
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1" // nothing listens
	target.state.Store(StateStopped)

	s := ForListeningPort("6379/tcp").WithStartupTimeout(30 * time.Second)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error when container stopped")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; want fail-fast, not full timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error = %v, want mention of stopped container", err)
	}
}

func TestForListeningPortFailsFastWhenContainerPaused(t *testing.T) {
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1" // nothing listens
	target.state.Store(StatePaused)

	s := ForListeningPort("6379/tcp").WithStartupTimeout(30 * time.Second)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error when container is paused")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; want fail-fast, not full timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "paused") {
		t.Errorf("error = %v, want mention of paused container", err)
	}
}

func TestPollRetriesCreatedUntilRunning(t *testing.T) {
	target := &firstStateTarget{fakeTarget: newFakeTarget(), first: StateCreated}
	checks := 0
	err := poll(
		context.Background(),
		options{startupTimeout: 2500 * time.Millisecond, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error {
			checks++
			if target.state.Load() != StateRunning {
				return errors.New("not ready")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if target.stateCalls.Load() < 2 {
		t.Fatalf("State calls = %d, want Created -> Running transition", target.stateCalls.Load())
	}
}

func TestPollRetriesRestartingUntilRunning(t *testing.T) {
	target := &firstStateTarget{fakeTarget: newFakeTarget(), first: StateRestarting}
	checks := 0
	err := poll(
		context.Background(),
		options{startupTimeout: 2500 * time.Millisecond, pollInterval: time.Millisecond},
		target,
		"wait for test",
		func(context.Context) error {
			checks++
			if target.state.Load() != StateRunning {
				return errors.New("not ready")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if checks < 2 {
		t.Fatalf("checks = %d, want retry after Restarting", checks)
	}
}

func TestPollRetriesUnknownAndTransientInspectErrorUntilTimeout(t *testing.T) {
	cases := map[string]error{
		"unknown state": nil,
		"inspect error": errors.New("temporary inspect failure"),
	}
	for name, stateErr := range cases {
		t.Run(name, func(t *testing.T) {
			target := newFakeTarget()
			target.state.Store(StateUnknown)
			target.stateErr = stateErr
			checks := 0
			err := poll(
				context.Background(),
				options{startupTimeout: 100 * time.Millisecond, pollInterval: 20 * time.Millisecond},
				target,
				"wait for test",
				func(context.Context) error {
					checks++
					return errors.New("not ready")
				},
			)
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("poll error = %v, want timeout", err)
			}
			if checks < 3 {
				t.Errorf("checks = %d, want retries until timeout", checks)
			}
		})
	}
}

func TestTerminalWaitStatePolicy(t *testing.T) {
	cases := map[State]bool{
		StateCreated:    false,
		StateRunning:    false,
		StateStopping:   true,
		StateRestarting: false,
		StateUnknown:    false,
		StateStopped:    true,
		StatePaused:     true,
	}
	for state, want := range cases {
		if got := terminalWaitState(state); got != want {
			t.Errorf("terminalWaitState(%q) = %t, want %t", state, got, want)
		}
	}
}

func TestForLogFindsSubstring(t *testing.T) {
	target := newFakeTarget()
	pr, pw := io.Pipe()
	target.logs = pr
	go func() {
		fmt.Fprintln(pw, "starting up")
		fmt.Fprintln(pw, "Ready to accept connections")
	}()

	s := ForLog("Ready to accept connections").WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForLogCountsOccurrences(t *testing.T) {
	target := newFakeTarget()
	pr, pw := io.Pipe()
	target.logs = pr
	go func() {
		fmt.Fprintln(pw, "ready")
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintln(pw, "ready")
	}()

	s := ForLog("ready").WithOccurrence(2).WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForLogAsRegexp(t *testing.T) {
	target := newFakeTarget()
	pr, pw := io.Pipe()
	target.logs = pr
	go fmt.Fprintln(pw, "listening on port 6379")

	s := ForLog(`listening on port \d+`).AsRegexp().WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForLogTimesOutWhenPatternNeverAppears(t *testing.T) {
	target := newFakeTarget()
	pr, _ := io.Pipe() // never written, never closed
	target.logs = pr

	s := ForLog("never").WithStartupTimeout(300 * time.Millisecond)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestForHTTPMatchesStatusCode(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")

	s := ForHTTP("/health").WithStartupTimeout(5 * time.Second).WithPollInterval(20 * time.Millisecond)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if hits.Load() < 3 {
		t.Errorf("hits = %d, want >= 3 (polling)", hits.Load())
	}
}

func TestForHTTPCustomStatusMatcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	target := newFakeTarget()
	target.endpoint = strings.TrimPrefix(srv.URL, "http://")

	s := ForHTTP("/").
		WithStatusCodeMatcher(func(code int) bool { return code == http.StatusUnauthorized }).
		WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForExecWaitsForExitCode(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 0

	s := ForExec([]string{"pg_isready"}).WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.execCalls.Load() == 0 {
		t.Error("exec never called")
	}
}

func TestForExecTimesOutOnPersistentFailure(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1

	s := ForExec([]string{"pg_isready"}).WithStartupTimeout(300 * time.Millisecond).WithPollInterval(50 * time.Millisecond)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestForListeningPortProbesStateDuringPoll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	target := newFakeTarget()
	target.endpoint = addr

	s := ForListeningPort("6379/tcp").
		WithStartupTimeout(2500 * time.Millisecond).
		WithPollInterval(50 * time.Millisecond)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout error")
	}
	// stateCheckInterval is 1s; a 2.5s poll should probe state a few times.
	if n := target.stateCalls.Load(); n < 2 || n > 5 {
		t.Errorf("State calls = %d, want 2..5 during connection poll", n)
	}
}

func TestForExecProbesStateDuringPoll(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1

	// Default ForExec interval is 250ms; a 3s timeout should exec a
	// modest number of times and inspect state at the bounded cadence.
	s := ForExec([]string{"pg_isready"}).WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout error")
	}
	if n := target.stateCalls.Load(); n < 3 || n > 5 {
		t.Errorf("State calls = %d, want periodic checks plus final classification", n)
	}
	// 3s / 250ms ≈ 12 intervals plus the initial check → ~13; allow slack.
	if n := target.execCalls.Load(); n < 10 || n > 16 {
		t.Errorf("exec calls = %d, want 10..16 with 250ms default interval", n)
	}
}

func TestForExecFailsImmediatelyOnLaunchError(t *testing.T) {
	target := newFakeTarget()
	target.execErr = &exec.Error{Name: "container", Err: errors.New("executable file not found in $PATH")}

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(30 * time.Second).
		WithPollInterval(50 * time.Millisecond)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; want immediate fail on launch error", elapsed)
	}
	if target.execCalls.Load() != 1 {
		t.Errorf("exec calls = %d, want 1 (no retry)", target.execCalls.Load())
	}
	if target.stateCalls.Load() != 1 {
		t.Errorf("State calls = %d, want 1 initial lifecycle check", target.stateCalls.Load())
	}
	if !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("error = %v, want launch failure", err)
	}
}

func TestForExecRetriesTransientErrors(t *testing.T) {
	target := newFakeTarget()
	target.execErr = errors.New("temporary exec failure")

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(400 * time.Millisecond).
		WithPollInterval(50 * time.Millisecond)
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if n := target.execCalls.Load(); n < 3 {
		t.Errorf("exec calls = %d, want >= 3 (retries)", n)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want timeout", err)
	}
}

func TestForExecReportsStoppedAtTimeout(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1
	target.state.Store(StateStopped)

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(200 * time.Millisecond).
		WithPollInterval(40 * time.Millisecond)
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error = %v, want stopped container", err)
	}
}

func TestForExecFinalStateProbeRespectsCallerCancel(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(30 * time.Second).
		WithPollInterval(20 * time.Millisecond)
	err := s.WaitUntilReady(ctx, target)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if target.stateCalls.Load() != 1 {
		t.Errorf("State calls = %d, want only the initial lifecycle check", target.stateCalls.Load())
	}
}

func TestForExecFinalStateProbeIsBounded(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1
	// State ignores progress until its context ends; without a bound
	// on the diagnostic probe this would hang for queryTimeout.
	slow := &slowStateTarget{fakeTarget: target, block: 30 * time.Second}

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(150 * time.Millisecond).
		WithPollInterval(40 * time.Millisecond)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), slow)
	if err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; want bounded final State probe", elapsed)
	}
}

// slowStateTarget blocks in State until ctx ends or block elapses.
type slowStateTarget struct {
	*fakeTarget
	block time.Duration
}

func (s *slowStateTarget) State(ctx context.Context) (State, error) {
	s.stateCalls.Add(1)
	timer := time.NewTimer(s.block)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return StateUnknown, ctx.Err()
	case <-timer.C:
		return s.state.Load().(State), nil
	}
}

func TestForAllRunsStrategiesInOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	target := newFakeTarget()
	target.endpoint = ln.Addr().String()
	pr, pw := io.Pipe()
	target.logs = pr
	go fmt.Fprintln(pw, "ready")

	s := ForAll(
		ForLog("ready").WithStartupTimeout(2*time.Second),
		ForListeningPort("6379/tcp").WithStartupTimeout(2*time.Second),
	)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForAllFailsWhenAnyFails(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1

	s := ForAll(
		ForExec([]string{"false"}).WithStartupTimeout(200 * time.Millisecond).WithPollInterval(50 * time.Millisecond),
	)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want error")
	}
}

func TestForAnySucceedsWhenOneSucceeds(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 0
	target.endpoint = "127.0.0.1:1" // dead port

	s := ForAny(
		ForListeningPort("6379/tcp").WithStartupTimeout(2*time.Second).WithPollInterval(50*time.Millisecond),
		ForExec([]string{"true"}).WithStartupTimeout(2*time.Second),
	)
	if err := s.WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
}

func TestForAllWithStartupTimeout(t *testing.T) {
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1" // dead port

	s := ForAll(
		ForListeningPort("6379/tcp").WithStartupTimeout(10*time.Second).WithPollInterval(20*time.Millisecond),
		ForListeningPort("6380/tcp").WithStartupTimeout(10*time.Second).WithPollInterval(20*time.Millisecond),
	).WithStartupTimeout(100 * time.Millisecond)

	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error on timeout")
	}
	if !strings.Contains(err.Error(), "wait for all: startup timeout") {
		t.Errorf("error %q missing composite timeout context", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want <= 2s", elapsed)
	}
}

func TestForAnyWithStartupTimeout(t *testing.T) {
	target := newFakeTarget()
	target.endpoint = "127.0.0.1:1" // dead port

	s := ForAny(
		ForListeningPort("6379/tcp").WithStartupTimeout(10*time.Second).WithPollInterval(20*time.Millisecond),
		ForListeningPort("6380/tcp").WithStartupTimeout(10*time.Second).WithPollInterval(20*time.Millisecond),
	).WithStartupTimeout(100 * time.Millisecond)

	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v: want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want <= 2s", elapsed)
	}
}

func TestForExecRejectsEmptyCommand(t *testing.T) {
	target := newFakeTarget()
	s := ForExec(nil).WithStartupTimeout(60 * time.Second)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want error for empty command")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want immediate error", elapsed)
	}
}
