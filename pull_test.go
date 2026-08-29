package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// runParallelRuns starts n Runs of the same image concurrently and
// returns the errors indexed by goroutine.
func runParallelRuns(t *testing.T, n int, opts ...Option) []error {
	t.Helper()
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctr, err := Run(context.Background(), "redis:7-alpine", opts...)
			if err != nil {
				errs[i] = err
				return
			}
			_ = ctr.Terminate(context.Background())
		}()
	}
	wg.Wait()
	return errs
}

func TestRunPullMissingPullsOnceAcrossTenParallelRuns(t *testing.T) {
	f := newTestRunner()
	errs := runParallelRuns(t, 10, WithName("myctr"), withRunner(f), withEngine(dockerEngine{}))
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if f.pullCalls != 1 {
		t.Fatalf("pulls = %d, want 1", f.pullCalls)
	}
}

func TestRunPullMissingDoesNotPullWhenImagePresent(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	ctr := runTestContainer(t, f, withEngine(dockerEngine{}))
	_ = ctr

	if f.pullCalls != 0 {
		t.Errorf("pulls = %d, want 0", f.pullCalls)
	}
	if !slices.Contains(f.callWith("image"), "inspect") {
		t.Errorf("existence was not checked: %v", f.calls)
	}
}

func TestRunPullAlwaysPullsEveryRun(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	opts := []Option{WithName("myctr"), WithPullPolicy(PullAlways), withRunner(f), withEngine(dockerEngine{})}
	for i := range 2 {
		if _, err := Run(context.Background(), "redis:7-alpine", opts...); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if f.pullCalls != 2 {
		t.Errorf("pulls = %d, want 2", f.pullCalls)
	}
}

func TestRunPullNeverFailsBeforeRunWhenImageMissing(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(f), withEngine(dockerEngine{}))
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("error = %v, want ErrImageNotFound", err)
	}
	if !strings.Contains(err.Error(), "redis:7-alpine") {
		t.Errorf("error %v does not name the image", err)
	}
	if f.callWith("run") != nil {
		t.Errorf("run was called despite missing image: %v", f.calls)
	}
}

func TestRunPullNeverRunsWhenImagePresent(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	ctr := runTestContainer(t, f, WithPullPolicy(PullNever), withEngine(dockerEngine{}))
	_ = ctr

	if f.pullCalls != 0 {
		t.Errorf("pulls = %d, want 0", f.pullCalls)
	}
	if f.callWith("run") == nil {
		t.Error("run was not called")
	}
}

func TestWithPullPolicyRejectsUnknownValue(t *testing.T) {
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullPolicy(42)), withRunner(f))
	if err == nil {
		t.Fatal("want error for unknown pull policy")
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid policy: %v", f.calls)
	}
}

func TestRunReportsSystemNotRunningFromImageCheck(t *testing.T) {
	// A downed backend surfaces during the image existence check,
	// before any run command is built.
	f := &fakeRunner{systemUp: false}
	_, err := Run(context.Background(), "redis:7-alpine", WithName("myctr"), withRunner(f))
	if !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
	if f.callWith("run") != nil {
		t.Errorf("run was called despite downed backend: %v", f.calls)
	}
}

func TestDockerRunArgsNeverPullImplicitly(t *testing.T) {
	cfg := dockerTestConfig(t)
	args := dockerEngine{}.runArgs(cfg, "redis:7-alpine", "")
	i := slices.Index(args, "--pull")
	if i < 0 || i+1 >= len(args) || args[i+1] != "never" {
		t.Errorf("run args missing --pull never: %v", args)
	}
}

func TestImageMissingClassification(t *testing.T) {
	imageErr := func(stderr string) error {
		return &cli.CLIError{Args: []string{"image", "inspect", "x"}, ExitCode: 1, Stderr: stderr}
	}
	if !(dockerEngine{}).imageMissing(imageErr("Error response from daemon: No such image: redis:7-alpine")) {
		t.Error("docker: daemon not-found not classified as missing")
	}
	if (dockerEngine{}).imageMissing(imageErr("XPC connection error")) {
		t.Error("docker: transport error classified as missing")
	}
	if !(appleEngine{}).imageMissing(imageErr("image not found: redis:7-alpine")) {
		t.Error("apple: not-found not classified as missing")
	}
	if (appleEngine{}).imageMissing(imageErr("XPC connection error")) {
		t.Error("apple: transport error classified as missing")
	}
}

func TestParseImageExists(t *testing.T) {
	if (dockerEngine{}).parseImageExists([]byte(`[]`)) {
		t.Error("docker: empty array means absent")
	}
	if !(dockerEngine{}).parseImageExists([]byte(`[{"Id":"sha256:x"}]`)) {
		t.Error("docker: non-empty array means present")
	}
	if (appleEngine{}).parseImageExists([]byte(`[]`)) {
		t.Error("apple: empty array means absent")
	}
	if !(appleEngine{}).parseImageExists([]byte(testImageInspectJSON)) {
		t.Error("apple: non-empty array means present")
	}
}

const testImageInspectJSON = `[
  {
    "reference": "docker.io/library/redis:7-alpine",
    "descriptor": {"mediaType": "application/vnd.oci.image.index.v1+json"}
  }
]`

func TestFlightGroupAggregatesConcurrentCallers(t *testing.T) {
	var g flightGroup
	var calls int
	var mu sync.Mutex
	var waiting atomic.Int64
	const n = 50

	// The leader holds the flight open until every waiter has joined.
	release := make(chan struct{})
	go func() {
		_ = g.do(context.Background(), "key", func() error {
			mu.Lock()
			calls++
			mu.Unlock()
			deadline := time.Now().Add(5 * time.Second)
			for waiting.Load() < n && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			// A short settle window covers the gap between a
			// waiter's arrival count and its entry into do.
			time.Sleep(50 * time.Millisecond)
			close(release)
			return nil
		})
	}()

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waiting.Add(1)
			if err := g.do(context.Background(), "key", func() error { return nil }); err != nil {
				t.Errorf("do: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("executions = %d, want 1", calls)
	}
}

func TestFlightGroupLateCallersReexecuteAfterCompletion(t *testing.T) {
	// A caller arriving after a flight completed re-runs fn: for the
	// image flows that means a fresh existence check instead of a
	// stale result.
	var g flightGroup
	if err := g.do(context.Background(), "key", func() error { return nil }); err != nil {
		t.Fatalf("first: %v", err)
	}
	executed := false
	if err := g.do(context.Background(), "key", func() error {
		executed = true
		return nil
	}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if !executed {
		t.Error("late caller did not execute fn")
	}
}

func TestFlightGroupPropagatesErrorToWaitersAndRetries(t *testing.T) {
	var g flightGroup
	first := g.do(context.Background(), "key", func() error {
		return fmt.Errorf("boom")
	})
	if first == nil || first.Error() != "boom" {
		t.Fatalf("first error = %v", first)
	}
	// A completed failed flight must not be reused; the next call
	// executes again.
	second := g.do(context.Background(), "key", func() error { return nil })
	if second != nil {
		t.Fatalf("second error = %v, want nil", second)
	}
}

func TestFlightGroupCancelledWaiterDoesNotAffectLeader(t *testing.T) {
	var g flightGroup
	release := make(chan struct{})
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- g.do(context.Background(), "key", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.do(ctx, "key", func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Errorf("leader error = %v", err)
	}
}
