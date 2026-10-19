package wait

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// blockingLogStream models a custom Target whose Close does not unblock
// an in-flight Read. FollowLogs callers must still return at the wait
// deadline instead of hanging on the scanner.
type blockingLogStream struct {
	mu       sync.Mutex
	unblock  chan struct{}
	once     sync.Once
	prefix   []byte
	inSettle chan struct{}
	settled  bool
}

func newBlockingLogStream(prefix string) *blockingLogStream {
	return &blockingLogStream{
		unblock:  make(chan struct{}),
		prefix:   []byte(prefix),
		inSettle: make(chan struct{}),
	}
}

func (s *blockingLogStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.prefix) > 0 {
		n := copy(p, s.prefix)
		s.prefix = s.prefix[n:]
		s.mu.Unlock()
		if n > 0 {
			return n, nil
		}
		s.mu.Lock()
	}
	if !s.settled {
		s.settled = true
		close(s.inSettle)
	}
	s.mu.Unlock()
	<-s.unblock
	return 0, io.EOF
}

func (s *blockingLogStream) Close() error { return nil }

func (s *blockingLogStream) release() {
	s.once.Do(func() { close(s.unblock) })
}

func TestForLogReturnsWhenCloseDoesNotUnblockRead(t *testing.T) {
	stream := newBlockingLogStream("")
	t.Cleanup(stream.release)
	target := newFakeTarget()
	target.logs = stream

	started := time.Now()
	err := ForLog("never").WithStartupTimeout(200*time.Millisecond).WaitUntilReady(context.Background(), target)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	if elapsed > time.Second {
		t.Fatalf("WaitUntilReady hung for %v after a non-cooperative Close", elapsed)
	}
}

func TestForLogPreservesMatchWhenSettleHitsDeadline(t *testing.T) {
	stream := newBlockingLogStream("ready\n")
	t.Cleanup(stream.release)
	target := newFakeTarget()
	target.logs = stream

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- ForLog("ready").WithStartupTimeout(2*time.Second).WaitUntilReady(ctx, target)
	}()

	select {
	case <-stream.inSettle:
	case err := <-errCh:
		t.Fatalf("wait returned before settle started: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for settle to start after the match")
	}
	// Cancel only after the matching line has been consumed and settle
	// has begun its diagnostic read, so a deadline during the settle
	// window cannot hide the already-observed pattern.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("observed match was dropped by settle delay: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitUntilReady hung after the wait context ended during settle")
	}
}

// cancelThenMatchStream unblocks only after the wait context ends and then
// returns a matching line. That late line must not count as readiness.
type cancelThenMatchStream struct {
	ctx  context.Context
	mu   sync.Mutex
	sent bool
}

func (s *cancelThenMatchStream) Read(p []byte) (int, error) {
	<-s.ctx.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent {
		return 0, io.EOF
	}
	s.sent = true
	return copy(p, []byte("ready\n")), nil
}

func (s *cancelThenMatchStream) Close() error { return nil }

type cancelThenMatchTarget struct {
	*fakeTarget
}

func (t *cancelThenMatchTarget) FollowLogs(ctx context.Context) (io.ReadCloser, error) {
	return &cancelThenMatchStream{ctx: ctx}, nil
}

func TestForLogRejectsMatchFirstObservedAfterCancel(t *testing.T) {
	target := &cancelThenMatchTarget{fakeTarget: newFakeTarget()}
	err := ForLog("ready").WithStartupTimeout(40*time.Millisecond).WaitUntilReady(context.Background(), target)
	if err == nil {
		t.Fatal("accepted a match first observed after the wait deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
}

func TestNonContextCauseDropsDeadlineButKeepsCLIFailure(t *testing.T) {
	if cause := nonContextCause(context.DeadlineExceeded); cause != nil {
		t.Fatalf("deadline-only cause = %v, want nil", cause)
	}
	cliErr := errors.New("logs follow exited 1")
	got := nonContextCause(joinNonNil(context.Canceled, cliErr))
	if !errors.Is(got, cliErr) {
		t.Fatalf("joined cause = %v, want CLI failure", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("joined cause still reports cancellation: %v", got)
	}
}
