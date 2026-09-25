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
	"sync"
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

type terminalLogStream struct {
	io.ReadCloser
	done     chan struct{}
	terminal error
}

func (s *terminalLogStream) Close() error          { return s.ReadCloser.Close() }
func (s *terminalLogStream) Done() <-chan struct{} { return s.done }
func (s *terminalLogStream) TerminalError() error  { return s.terminal }

func TestForLogObservesTerminalStreamErrorAfterMatch(t *testing.T) {
	terminal := errors.New("logs CLI exited after emitting match")
	done := make(chan struct{})
	close(done)
	target := newFakeTarget()
	target.logs = &terminalLogStream{
		ReadCloser: io.NopCloser(strings.NewReader("ready\n")),
		done:       done,
		terminal:   terminal,
	}

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, terminal) {
		t.Fatalf("ForLog error = %v, want terminal stream error", err)
	}
}

type delayedTerminalLogReader struct {
	reader    io.Reader
	done      chan struct{}
	closeOnce sync.Once
	terminal  error
}

func (r *delayedTerminalLogReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}
func (r *delayedTerminalLogReader) Close() error          { return nil }
func (r *delayedTerminalLogReader) Done() <-chan struct{} { return r.done }
func (r *delayedTerminalLogReader) TerminalError() error  { return r.terminal }
func (r *delayedTerminalLogReader) finish()               { r.closeOnce.Do(func() { close(r.done) }) }

func TestForLogSettlesTerminalErrorAfterMatch(t *testing.T) {
	terminal := errors.New("terminal CLI failure after match")
	reader := &delayedTerminalLogReader{
		reader:   strings.NewReader("ready\n"),
		done:     make(chan struct{}),
		terminal: terminal,
	}
	time.AfterFunc(time.Millisecond, reader.finish)
	target := newFakeTarget()
	target.logs = reader

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, terminal) {
		t.Fatalf("ForLog error = %v, want terminal error", err)
	}
}

type delayedTerminalErrorStream struct {
	reader        io.Reader
	done          chan struct{}
	terminalReady chan struct{}
	terminal      error
}

func (s *delayedTerminalErrorStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}
func (s *delayedTerminalErrorStream) Close() error          { return nil }
func (s *delayedTerminalErrorStream) Done() <-chan struct{} { return s.done }
func (s *delayedTerminalErrorStream) TerminalError() error {
	select {
	case <-s.terminalReady:
		return s.terminal
	default:
		return nil
	}
}

func TestForLogPreservesDelayedTerminalErrorAfterMatch(t *testing.T) {
	terminal := errors.New("terminal CLI failure arrived after settle")
	stream := &delayedTerminalErrorStream{
		reader:        strings.NewReader("ready\n"),
		done:          make(chan struct{}),
		terminalReady: make(chan struct{}),
		terminal:      terminal,
	}
	time.AfterFunc(time.Millisecond, func() { close(stream.done) })
	time.AfterFunc(20*time.Millisecond, func() { close(stream.terminalReady) })
	target := newFakeTarget()
	target.logs = stream

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, terminal) {
		t.Fatalf("ForLog error = %v, want delayed terminal error", err)
	}
}

type cancelDelayedTerminalStream struct {
	data          []byte
	dataRead      chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	done          chan struct{}
	terminalReady chan struct{}
	readErr       error
	terminal      error
}

func (s *cancelDelayedTerminalStream) Read(p []byte) (int, error) {
	if len(s.data) > 0 {
		n := copy(p, s.data)
		s.data = s.data[n:]
		close(s.dataRead)
		return n, nil
	}
	<-s.closed
	return 0, s.readErr
}
func (s *cancelDelayedTerminalStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		close(s.done)
	})
	return nil
}
func (s *cancelDelayedTerminalStream) Done() <-chan struct{} { return s.done }
func (s *cancelDelayedTerminalStream) TerminalError() error {
	select {
	case <-s.terminalReady:
		return s.terminal
	default:
		return nil
	}
}

func TestForLogJoinsCancellationWithDelayedReaderAndTerminalErrors(t *testing.T) {
	readErr := errors.New("reader failed after cancellation")
	terminal := errors.New("terminal failed after cancellation")
	stream := &cancelDelayedTerminalStream{
		data:          []byte("ready\n"),
		dataRead:      make(chan struct{}),
		closed:        make(chan struct{}),
		done:          make(chan struct{}),
		terminalReady: make(chan struct{}),
		readErr:       readErr,
		terminal:      terminal,
	}
	time.AfterFunc(20*time.Millisecond, func() { close(stream.terminalReady) })
	target := newFakeTarget()
	target.logs = stream
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
	}()
	<-stream.dataRead
	cancel()

	err := <-result
	if !errors.Is(err, context.Canceled) || !errors.Is(err, readErr) || !errors.Is(err, terminal) {
		t.Fatalf("ForLog error = %v, want context, reader, and terminal errors", err)
	}
}

func TestForLogPreservesTerminalAndContextErrors(t *testing.T) {
	terminal := errors.New("terminal CLI failure")
	done := make(chan struct{})
	close(done)
	target := newFakeTarget()
	target.logs = &terminalLogStream{
		ReadCloser: io.NopCloser(strings.NewReader("ready\n")),
		done:       done,
		terminal:   terminal,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
	if !errors.Is(err, terminal) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ForLog error = %v, want terminal and context errors", err)
	}
}

type errorAfterLogReader struct {
	io.Reader
	err error
}

func (r *errorAfterLogReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}
func (r *errorAfterLogReader) Close() error { return nil }

func TestForLogPreservesReaderErrorAfterMatch(t *testing.T) {
	readErr := errors.New("log reader failed after matching line")
	target := newFakeTarget()
	target.logs = &errorAfterLogReader{Reader: strings.NewReader("ready\n"), err: readErr}

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, readErr) {
		t.Fatalf("ForLog error = %v, want reader error", err)
	}
}

type delayedReaderErrorStream struct {
	data      []byte
	closeOnce sync.Once
	closed    chan struct{}
	err       error
}

func (s *delayedReaderErrorStream) Read(p []byte) (int, error) {
	if len(s.data) > 0 {
		n := copy(p, s.data)
		s.data = s.data[n:]
		return n, nil
	}
	<-s.closed
	return 0, s.err
}

func (s *delayedReaderErrorStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func TestForLogPreservesDelayedReaderErrorAfterMatch(t *testing.T) {
	readErr := errors.New("log reader failed after delayed close")
	target := newFakeTarget()
	target.logs = &delayedReaderErrorStream{
		data:   []byte("ready\n"),
		closed: make(chan struct{}),
		err:    readErr,
	}

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, readErr) {
		t.Fatalf("ForLog error = %v, want delayed reader error", err)
	}
}

func TestForLogDoesNotProbeAfterReaderFailure(t *testing.T) {
	readErr := errors.New("log reader failed")
	target := newFakeTarget()
	target.logs = &errorAfterLogReader{Reader: strings.NewReader("not ready\n"), err: readErr}

	err := ForLog("ready").WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), target)
	if !errors.Is(err, readErr) {
		t.Fatalf("ForLog error = %v, want reader error", err)
	}
	if calls := target.runningCalls.Load(); calls != 0 {
		t.Fatalf("Running calls after reader failure = %d, want 0", calls)
	}
}

type forLogCancelProbeTarget struct {
	*fakeTarget
	cancel   context.CancelFunc
	probeErr error
}

func (t *forLogCancelProbeTarget) Running(context.Context) (bool, error) {
	t.cancel()
	return false, t.probeErr
}

func TestForLogJoinsCallerCancellationAfterFinalProbe(t *testing.T) {
	probeErr := errors.New("state probe failed")
	ctx, cancel := context.WithCancel(context.Background())
	target := &forLogCancelProbeTarget{fakeTarget: newFakeTarget(), cancel: cancel, probeErr: probeErr}
	target.logs = io.NopCloser(strings.NewReader("not ready\n"))

	err := ForLog("never").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
	if !errors.Is(err, probeErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ForLog error = %v, want probe and caller cancellation", err)
	}
}

type forLogDeadlineProbeTarget struct {
	*fakeTarget
}

func (t *forLogDeadlineProbeTarget) Running(ctx context.Context) (bool, error) {
	t.runningCalls.Add(1)
	<-ctx.Done()
	return false, ctx.Err()
}

func TestForLogJoinsCallerDeadlineAfterFinalProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	target := &forLogDeadlineProbeTarget{fakeTarget: newFakeTarget()}
	target.logs = io.NopCloser(strings.NewReader("not ready\n"))

	err := ForLog("never").WithStartupTimeout(time.Second).WaitUntilReady(ctx, target)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForLog error = %v, want caller deadline", err)
	}
	if target.runningCalls.Load() == 0 {
		t.Fatal("final Running probe was not called")
	}
}

type cancelAwareLogStream struct {
	ctx    context.Context
	closed chan struct{}
}

func (s *cancelAwareLogStream) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s *cancelAwareLogStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

type forLogProbeTarget struct {
	*fakeTarget
	streamStarted chan struct{}
}

func (t *forLogProbeTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	stream := &cancelAwareLogStream{ctx: ctx, closed: make(chan struct{})}
	close(t.streamStarted)
	return stream, nil
}

func TestForAnyCancellationDoesNotStartDetachedRunningProbe(t *testing.T) {
	target := &forLogProbeTarget{fakeTarget: newFakeTarget(), streamStarted: make(chan struct{})}
	loser := ForLog("never").WithStartupTimeout(5 * time.Second)
	winner := &issue116ForAnyWinner{loserStarted: target.streamStarted}

	if err := ForAny(loser, winner).WaitUntilReady(context.Background(), target); err != nil {
		t.Fatalf("ForAny: %v", err)
	}
	if calls := target.runningCalls.Load(); calls != 0 {
		t.Fatalf("Running calls after canceled ForLog loser = %d, want 0", calls)
	}
}

func TestForLogTimesOutWhenPatternNeverAppears(t *testing.T) {
	target := newFakeTarget()
	pr, _ := io.Pipe() // never written, never closed
	target.logs = pr

	s := ForLog("never").WithStartupTimeout(300 * time.Millisecond)
	err := s.WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
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

type issue116ForAnyLoser struct {
	started  chan struct{}
	returned chan struct{}
}

func (s *issue116ForAnyLoser) WaitUntilReady(ctx context.Context, _ Target) error {
	close(s.started)
	<-ctx.Done()
	close(s.returned)
	return errors.New("losing exec was canceled")
}

type issue116ForAnyWinner struct {
	loserStarted <-chan struct{}
}

func (s *issue116ForAnyWinner) WaitUntilReady(ctx context.Context, _ Target) error {
	select {
	case <-s.loserStarted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestForAnyDrainsCanceledLosersBeforeSuccess(t *testing.T) {
	loser := &issue116ForAnyLoser{
		started:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	winner := &issue116ForAnyWinner{loserStarted: loser.started}

	if err := ForAny(loser, winner).WaitUntilReady(context.Background(), newFakeTarget()); err != nil {
		t.Fatalf("ForAny success = %v, want nil", err)
	}
	select {
	case <-loser.returned:
		// The success result was returned only after the canceled loser
		// completed its lifecycle path.
	default:
		t.Fatal("ForAny returned before the canceled loser finished")
	}
}

type issue116NonCooperativeStrategy struct {
	started chan struct{}
	release chan struct{}
}

func (s *issue116NonCooperativeStrategy) WaitUntilReady(context.Context, Target) error {
	close(s.started)
	<-s.release
	return nil
}

func TestForAnyBoundsNonCooperativeLoserDrain(t *testing.T) {
	loser := &issue116NonCooperativeStrategy{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { close(loser.release) })

	winner := &issue116ForAnyWinner{loserStarted: loser.started}
	started := time.Now()
	if err := ForAny(loser, winner).WaitUntilReady(context.Background(), newFakeTarget()); err != nil {
		t.Fatalf("ForAny success = %v, want nil", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("ForAny success took %v with non-cooperative loser", elapsed)
	}
}

func TestForAnyBoundsNonCooperativeDrainAfterTimeout(t *testing.T) {
	strategy := &issue116NonCooperativeStrategy{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { close(strategy.release) })

	started := time.Now()
	err := ForAny(strategy).WithStartupTimeout(20*time.Millisecond).WaitUntilReady(context.Background(), newFakeTarget())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForAny error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("ForAny timeout took %v with non-cooperative strategy", elapsed)
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
