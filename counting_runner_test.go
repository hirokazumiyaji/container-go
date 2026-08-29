package container

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// countingRunner wraps a cli.Runner and counts every invocation. The
// benchmark scenarios (#17) and the regression tests for the
// subprocess-reduction issues (#18, #19, #20) use it to assert how
// many CLI child processes a code path spawns.
type countingRunner struct {
	inner cli.Runner
	calls atomic.Int64
}

func newCountingRunner(inner cli.Runner) *countingRunner {
	return &countingRunner{inner: inner}
}

func (r *countingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.calls.Add(1)
	return r.inner.Run(ctx, args...)
}

// Stream forwards to the inner runner when it supports streaming and
// counts the spawned child process.
func (r *countingRunner) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	s, ok := r.inner.(cli.Streamer)
	if !ok {
		return nil, fmt.Errorf("countingRunner: inner runner does not support streaming")
	}
	r.calls.Add(1)
	return s.Stream(ctx, args...)
}

func (r *countingRunner) count() int64 { return r.calls.Load() }

// TestCountingRunnerCountsEveryCall pins the counter to a known CLI
// call sequence.
func TestCountingRunnerCountsEveryCall(t *testing.T) {
	f := newTestRunner()
	r := newCountingRunner(f)

	ctr := runTestContainer(t, r, WithExposedPorts("6379/tcp"))

	// Expected calls: run, then the eager first inspect cachedInfo
	// performs right after start. (#20 will drop the second.)
	if got := r.count(); got != 2 {
		t.Fatalf("after Run: calls = %d, want 2", got)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	// Endpoint resolves from the cached inspect; no extra spawn.
	if got := r.count(); got != 2 {
		t.Fatalf("after Endpoint: calls = %d, want 2", got)
	}

	// The wrapper forwards results unchanged.
	if got := len(f.calls); int64(got) != r.count() {
		t.Errorf("inner calls = %d, counted = %d", got, r.count())
	}
}
