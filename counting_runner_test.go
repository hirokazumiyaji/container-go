package container

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
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

	// Expected calls: image inspect (present, no pull) and run. The
	// first container inspect is deferred until connection info is needed.
	if got := r.count(); got != 2 {
		t.Fatalf("after Run: calls = %d, want 2", got)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	// Endpoint triggers the deferred inspect once; later reads reuse it.
	if got := r.count(); got != 3 {
		t.Fatalf("after Endpoint: calls = %d, want 3", got)
	}
	if _, err := ctr.Endpoint(context.Background(), "6379/tcp"); err != nil {
		t.Fatalf("Endpoint again: %v", err)
	}
	if got := r.count(); got != 3 {
		t.Fatalf("after cached Endpoint: calls = %d, want 3", got)
	}

	// The wrapper forwards results unchanged.
	if got := len(f.calls); int64(got) != r.count() {
		t.Errorf("inner calls = %d, counted = %d", got, r.count())
	}
}

// TestRunForLogSkipsInitialInspect pins that a successful ForLog wait
// does not pay for an eager post-start inspect.
func TestRunForLogSkipsInitialInspect(t *testing.T) {
	inner := &streamRunner{
		fakeRunner: newTestRunner(),
		streamData: "Ready to accept connections\n",
	}
	inner.imagePresent = true
	r := newCountingRunner(inner)

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}),
		WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// image inspect + run + generation-verifying inspect + logs stream.
	if got := r.count(); got != 4 {
		t.Fatalf("after ForLog Run: calls = %d, want 4", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inspectCount := 0
	for _, args := range r.args {
		if len(args) > 0 && args[0] == "inspect" {
			inspectCount++
		}
	}
	if inspectCount != 1 {
		t.Fatalf("container inspect calls = %d, want one generation check: %v", inspectCount, r.args)
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
