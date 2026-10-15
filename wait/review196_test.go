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
	unblock chan struct{}
	once    sync.Once
	prefix  []byte
	sent    bool
}

func newBlockingLogStream(prefix string) *blockingLogStream {
	return &blockingLogStream{
		unblock: make(chan struct{}),
		prefix:  []byte(prefix),
	}
}

func (s *blockingLogStream) Read(p []byte) (int, error) {
	if len(s.prefix) > 0 && !s.sent {
		s.sent = true
		n := copy(p, s.prefix)
		s.prefix = s.prefix[n:]
		if n > 0 {
			return n, nil
		}
	}
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
	err := ForLog("never").WithStartupTimeout(40*time.Millisecond).WaitUntilReady(context.Background(), target)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("WaitUntilReady hung for %v after a non-cooperative Close", elapsed)
	}
}

func TestForLogPreservesMatchWhenSettleHitsDeadline(t *testing.T) {
	stream := newBlockingLogStream("ready\n")
	t.Cleanup(stream.release)
	target := newFakeTarget()
	target.logs = stream

	// Shorter than the diagnostic settle windows, and arriving in the
	// final milliseconds of the wait budget, must not invalidate a match
	// that was already observed.
	err := ForLog("ready").WithStartupTimeout(30*time.Millisecond).WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("observed match was dropped by settle delay: %v", err)
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
