package cli

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

// TestStreamRetainsOutputForSlowReader covers a producer that outruns the
// public reader. The ordered queue must apply backpressure rather than drop
// chunks once its bounded capacity is reached.
func TestStreamRetainsOutputForSlowReader(t *testing.T) {
	// Emit many lines quickly.
	script := "i=0; while [ $i -lt 4000 ]; do echo line-$i; i=$((i+1)); done"
	r := &ExecRunner{Binary: writeStub(t, script)}
	stream, err := r.Stream(context.Background(), "logs", "x")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	// Stall before reading anything, so the queue backs up.
	time.Sleep(300 * time.Millisecond)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 4000 {
		t.Errorf("dropped %d lines of child output", 4000-len(lines))
	}
	if len(lines) > 0 && !strings.HasPrefix(lines[0], "line-") {
		t.Errorf("first line = %q, want line-0 (ordering/content corrupted)", lines[0])
	}
}
