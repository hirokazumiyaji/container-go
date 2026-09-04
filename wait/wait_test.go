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
	endpoint     string
	running      atomic.Bool
	runningCalls atomic.Int32
	logs         io.ReadCloser
	execCode     int
	execErr      error
	execCalls    atomic.Int32
}

func newFakeTarget() *fakeTarget {
	t := &fakeTarget{logs: io.NopCloser(strings.NewReader(""))}
	t.running.Store(true)
	return t
}

func (f *fakeTarget) Endpoint(_ context.Context, port string) (string, error) {
	if f.endpoint == "" {
		return "", errors.New("no endpoint configured")
	}
	return f.endpoint, nil
}

func (f *fakeTarget) Running(_ context.Context) (bool, error) {
	f.runningCalls.Add(1)
	return f.running.Load(), nil
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
	target.running.Store(false)

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

func TestForListeningPortProbesRunningDuringPoll(t *testing.T) {
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
	// stateCheckInterval is 1s; a 2.5s poll should probe Running a few times.
	if n := target.runningCalls.Load(); n < 2 || n > 5 {
		t.Errorf("Running calls = %d, want 2..5 during connection poll", n)
	}
}

func TestForExecSkipsRunningDuringPoll(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1

	// Default ForExec interval is 250ms; a 3s timeout should exec a
	// modest number of times and only inspect Running once at the end.
	s := ForExec([]string{"pg_isready"}).WithStartupTimeout(3 * time.Second)
	if err := s.WaitUntilReady(context.Background(), target); err == nil {
		t.Fatal("want timeout error")
	}
	if n := target.runningCalls.Load(); n != 1 {
		t.Errorf("Running calls = %d, want 1 (final classification only)", n)
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
	if target.runningCalls.Load() != 0 {
		t.Errorf("Running calls = %d, want 0 on fatal check error", target.runningCalls.Load())
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
	target.running.Store(false)

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

func TestForExecFinalRunningProbeRespectsCallerCancel(t *testing.T) {
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
	if target.runningCalls.Load() != 0 {
		t.Errorf("Running calls = %d, want 0 when caller cancels", target.runningCalls.Load())
	}
}

func TestForExecFinalRunningProbeIsBounded(t *testing.T) {
	target := newFakeTarget()
	target.execCode = 1
	// Running ignores progress until its context ends; without a bound
	// on the diagnostic probe this would hang for queryTimeout.
	slow := &slowRunningTarget{fakeTarget: target, block: 30 * time.Second}

	s := ForExec([]string{"pg_isready"}).
		WithStartupTimeout(150 * time.Millisecond).
		WithPollInterval(40 * time.Millisecond)
	start := time.Now()
	err := s.WaitUntilReady(context.Background(), slow)
	if err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; want bounded final Running probe", elapsed)
	}
}

// slowRunningTarget blocks in Running until ctx ends or block elapses.
type slowRunningTarget struct {
	*fakeTarget
	block time.Duration
}

func (s *slowRunningTarget) Running(ctx context.Context) (bool, error) {
	s.runningCalls.Add(1)
	timer := time.NewTimer(s.block)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return s.running.Load(), nil
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
