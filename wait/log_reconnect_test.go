package wait

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// A follow stream can end between the bytes and the newline of a line. The
// reconnect replays the completed line, which must be counted once instead of
// being dropped behind the committed fragment.
func TestForLogReconnectCompletesSplitOccurrence(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("ready\nrea")),
		io.NopCloser(strings.NewReader("ready\nready\n")),
	}

	err := ForLog("ready").
		WithOccurrence(2).
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.logCalls; got != 2 {
		t.Fatalf("FollowLogs calls = %d, want the split line completed on reconnect", got)
	}
}

func TestForLogReconnectCompletesOnlyPartialLine(t *testing.T) {
	target := newLifecycleTarget(StateRunning)
	target.logStreams = []io.ReadCloser{
		io.NopCloser(strings.NewReader("rea")),
		io.NopCloser(strings.NewReader("ready\n")),
	}

	err := ForLog("ready").
		WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := target.logCalls; got != 2 {
		t.Fatalf("FollowLogs calls = %d, want the fragment completed on reconnect", got)
	}
}

// A rewritten history can diverge before the committed length. Every observed
// line still advances the append-only history, so the replay line count must
// never fall below the retained fingerprint ring.
func TestScanLogStreamKeepsReplayLineCountConsistent(t *testing.T) {
	first := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader("ready\none\ntwo\n")),
		func(line string) int { return strings.Count(line, "ready") },
		2,
		logReplay{},
	)
	if first.err != nil {
		t.Fatalf("first scan: %v", first.err)
	}
	if first.count != 1 || first.lineCount != 3 || len(first.lines) != 3 {
		t.Fatalf("first scan = count:%d lines:%d fingerprints:%d, want 1/3/3", first.count, first.lineCount, len(first.lines))
	}
	replay := logReplay{
		previous:      first.lines,
		previousStart: first.lineStart,
		previousLines: first.lineCount,
		count:         first.count,
		partial:       first.partial,
		partialCount:  first.partialCount,
	}

	second := scanLogStream(
		context.Background(),
		io.NopCloser(strings.NewReader("x\ny\nz\n")),
		func(string) int { return 0 },
		2,
		replay,
	)
	if second.err != nil {
		t.Fatalf("second scan: %v", second.err)
	}
	if !second.committed {
		t.Fatal("full-length rewritten history was not committed")
	}
	if second.lineCount < len(second.lines) {
		t.Fatalf("line count = %d, fingerprints = %d: replay ring outgrew the history", second.lineCount, len(second.lines))
	}
	if second.lineCount != 3 {
		t.Fatalf("line count = %d, want 3 observed lines", second.lineCount)
	}
}
