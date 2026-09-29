package integrationtest

import (
	"io"
	"strings"
	"testing"
	"time"
)

// A child that floods output before sending the marker must not lose the
// terminal signal. The channel holds one snapshot, so a queued non-terminal
// chunk used to make the terminal send take the default branch, and the caller
// then waited out the whole timeout for a child that had already reported.
//
// This is a scheduling race, so a single run proves little: against the
// pre-fix send it reproduces roughly one run in eight. Run it with -count to
// give it a real chance of catching a regression.
func TestReadReadyKeepsTerminalSignalUnderBurst(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	// Write far more than one snapshot's worth, then the marker, all without
	// yielding, so the reader publishes several chunks before finishing.
	go func() {
		for i := range 50 {
			_, _ = pw.Write([]byte(strings.Repeat("x", 4096) + "\n"))
			_ = i
		}
		_, _ = pw.Write([]byte("READY: myctr running\n"))
	}()

	done := make(chan error, 1)
	go func() {
		_, err := ReadReady(pr, 20*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadReady = %v, want nil for a child that reported READY", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("ReadReady blocked; the terminal signal was dropped")
	}
}

// The reported output on a timeout must be the newest the child wrote, not a
// stale snapshot. select picks randomly when both cases are ready, so a
// queued chunk has to be drained before returning.
func TestReadReadyTimeoutReportsNewestOutput(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	marker := "LASTLINE-MARKER"
	go func() {
		for i := range 20 {
			_, _ = pw.Write([]byte("line\n"))
			_ = i
		}
		_, _ = pw.Write([]byte(marker + "\n"))
		// Then go quiet forever without sending a readiness marker.
	}()

	out, err := ReadReady(pr, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("expected a timeout, got %q", out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("timeout output is missing the newest chunk: %q", out[len(out)-80:])
	}
}
