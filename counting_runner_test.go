package container

import (
	"context"
	"fmt"
	"io"
	"sync"
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

	mu   sync.Mutex
	args [][]string // args of each counted call
}

func newCountingRunner(inner cli.Runner) *countingRunner {
	return &countingRunner{inner: inner}
}

func (r *countingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.args = append(r.args, args)
	r.mu.Unlock()
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
	r.mu.Lock()
	r.args = append(r.args, args)
	r.mu.Unlock()
	return s.Stream(ctx, args...)
}

// External forwards the inner runner's externalness. Implementing
// cli.ExternalRunner keeps Run's reaper registration active around the
// counting scenarios, so they measure the production path; wrapping a
// test double stays non-external, like the double itself.
func (r *countingRunner) External() bool {
	er, ok := r.inner.(cli.ExternalRunner)
	return ok && er.External()
}

// ExternalBinary forwards the inner runner's binary, or "" when the
// inner runner is not external (Run then falls back to the engine
// default, which is unreachable because External reports false).
func (r *countingRunner) ExternalBinary() string {
	er, ok := r.inner.(cli.ExternalRunner)
	if !ok {
		return ""
	}
	return er.ExternalBinary()
}

func (r *countingRunner) count() int64 { return r.calls.Load() }

// TestCountingRunnerCountsEveryCall pins the counter to a known CLI
// call sequence.
func TestCountingRunnerCountsEveryCall(t *testing.T) {
	f := newTestRunner()
	r := newCountingRunner(f)

	// Seed the image into the fake store so the pull policy performs
	// only the existence check.
	f.imagePresent = true

	ctr := runTestContainer(t, r, WithExposedPorts("6379/tcp"))

	// Expected calls: image inspect (present, no pull), run, then the
	// eager first inspect cachedInfo performs right after start.
	// (#20 will drop the last one.)
	if got := r.count(); got != 3 {
		t.Fatalf("after Run: calls = %d, want 3", got)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	// Endpoint resolves from the cached inspect; no extra spawn.
	if got := r.count(); got != 3 {
		t.Fatalf("after Endpoint: calls = %d, want 3", got)
	}

	// The wrapper forwards results unchanged.
	if got := len(f.calls); int64(got) != r.count() {
		t.Errorf("inner calls = %d, counted = %d", got, r.count())
	}
}

// TestCountingRunnerExternalForwarding pins the cli.ExternalRunner
// forwarding: around the real ExecRunner the wrapper must stay
// external so Run keeps registering containers with the reaper, and
// around a test double it must not claim externalness.
func TestCountingRunnerExternalForwarding(t *testing.T) {
	double := newCountingRunner(newTestRunner())
	if double.External() {
		t.Error("countingRunner around a test double reports external, want false")
	}

	external := newCountingRunner(&cli.ExecRunner{Binary: "docker"})
	if !external.External() {
		t.Error("countingRunner around ExecRunner reports external = false, want true")
	}
	if got := external.ExternalBinary(); got != "docker" {
		t.Errorf("ExternalBinary = %q, want %q", got, "docker")
	}
}
