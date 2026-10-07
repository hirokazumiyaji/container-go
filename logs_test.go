package container

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
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

type blockingFollowStream struct {
	done chan struct{}
	once sync.Once
}

func (s *blockingFollowStream) Read([]byte) (int, error) {
	<-s.done
	return 0, io.EOF
}

func (s *blockingFollowStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

type followLockRunner struct {
	*fakeRunner
	stream *blockingFollowStream
}

func (r *followLockRunner) Stream(context.Context, ...string) (io.ReadCloser, error) {
	return r.stream, nil
}

func TestFollowLogsReleasesAppleLockBeforeProbes(t *testing.T) {
	runner := &followLockRunner{
		fakeRunner: newTestRunner(),
		stream:     &blockingFollowStream{done: make(chan struct{})},
	}
	ctr := runTestContainer(t, runner)
	stream, err := ctr.FollowLogs(context.Background())
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	defer stream.Close()

	probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := ctr.State(probeCtx)
	if err != nil {
		t.Fatalf("State while FollowLogs is open: %v", err)
	}
	if state != StateRunning {
		t.Fatalf("State = %s, want running", state)
	}
}

type endpointProbeStrategy struct{}

func (endpointProbeStrategy) WaitUntilReady(ctx context.Context, target wait.Target) error {
	_, err := target.Endpoint(ctx, "6379/tcp")
	return err
}

func TestForAnyCanProbeWhileFollowLogsIsOpen(t *testing.T) {
	runner := &followLockRunner{
		fakeRunner: newTestRunner(),
		stream:     &blockingFollowStream{done: make(chan struct{})},
	}
	ctr := runTestContainer(t, runner, WithExposedPorts("6379/tcp"))

	started := time.Now()
	err := wait.ForAny(
		wait.ForLog("never appears").WithStartupTimeout(5*time.Second),
		endpointProbeStrategy{},
	).WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), waitTarget{c: ctr})
	if err != nil {
		t.Fatalf("ForAny: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ForAny took %s while FollowLogs was open", elapsed)
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

func TestLogsWithOptionsHoldsLockUntilClose(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	rc, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	tryCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	unlock, err := lockName(tryCtx, ctr.id)
	if err == nil {
		unlock()
		t.Fatal("expected lockName to block or fail while Logs stream is open")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	openCtx, openCancel := context.WithTimeout(context.Background(), time.Second)
	defer openCancel()
	unlock, err = lockName(openCtx, ctr.id)
	if err != nil {
		t.Fatalf("lockName after Close: %v", err)
	}
	unlock()
}
