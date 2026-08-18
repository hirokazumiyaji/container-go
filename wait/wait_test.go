package wait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTarget implements Target for tests.
type fakeTarget struct {
	endpoint  string
	running   atomic.Bool
	logs      io.ReadCloser
	execCode  int
	execErr   error
	execCalls atomic.Int32
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

func (f *fakeTarget) Running(_ context.Context) (bool, error) { return f.running.Load(), nil }

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
