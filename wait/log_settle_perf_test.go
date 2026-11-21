package wait

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// liveStatusLogStream models a running `logs --follow` process: it yields a
// matching line, then blocks until Close while its process status stays open.
type liveStatusLogStream struct {
	mu         sync.Mutex
	prefix     []byte
	extraReads atomic.Int32
	done       chan struct{}
	closeOnce  sync.Once
}

func newLiveStatusLogStream(prefix string) *liveStatusLogStream {
	return &liveStatusLogStream{prefix: []byte(prefix), done: make(chan struct{})}
}

func (s *liveStatusLogStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.prefix) > 0 {
		n := copy(p, s.prefix)
		s.prefix = s.prefix[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()
	// A settlement read after the match is observable: it is the forced
	// idle window this regression test rejects.
	s.extraReads.Add(1)
	<-s.done
	return 0, io.EOF
}

func (s *liveStatusLogStream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

func (s *liveStatusLogStream) Done() <-chan struct{} { return s.done }

func (*liveStatusLogStream) TerminalError() error { return nil }

func TestForLogLiveStreamMatchDoesNotWaitForSettle(t *testing.T) {
	stream := newLiveStatusLogStream("ready\n")
	fake := newFakeTarget()
	fake.logs = stream

	err := ForLog("ready").WithStartupTimeout(2*time.Second).WaitUntilReady(context.Background(), fake)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if reads := stream.extraReads.Load(); reads != 0 {
		t.Fatalf("settlement issued %d reads after a live match, want none", reads)
	}
}

type liveSettleBenchTarget struct{}

func (liveSettleBenchTarget) Endpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}
func (liveSettleBenchTarget) Running(context.Context) (bool, error) { return true, nil }
func (liveSettleBenchTarget) State(context.Context) (State, error)  { return StateRunning, nil }
func (liveSettleBenchTarget) ExecCommand(context.Context, []string) (int, error) {
	return 0, nil
}
func (liveSettleBenchTarget) FollowLogs(context.Context) (io.ReadCloser, error) {
	return newLiveStatusLogStream("ready\n"), nil
}

// BenchmarkForLogLiveStreamMatch measures the readiness latency after a
// match on a still-running follow stream. Before the live-stream fast path
// every successful wait paid the settlement idle window.
func BenchmarkForLogLiveStreamMatch(b *testing.B) {
	strategy := ForLog("ready").WithStartupTimeout(time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := strategy.WaitUntilReady(context.Background(), liveSettleBenchTarget{}); err != nil {
			b.Fatalf("WaitUntilReady: %v", err)
		}
	}
}
