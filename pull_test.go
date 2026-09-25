package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
	f.binary = "docker"
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
	f.binary = "docker"
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
	f.binary = "docker"
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
	_, err := Run(context.Background(), "redis:7-alpine", WithName("myctr"), withRunner(f), withEngine(appleEngine{}))
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
	imageErr := func(binary, stderr string) error {
		return &cli.CLIError{Binary: binary, Args: []string{"image", "inspect", "redis:7-alpine"}, ExitCode: 1, Stderr: stderr}
	}
	if !(dockerEngine{}).imageMissing(imageErr("docker", "Error response from daemon: No such image: redis:7-alpine")) {
		t.Error("docker: daemon not-found not classified as missing")
	}
	if (dockerEngine{}).imageMissing(imageErr("docker", "XPC connection error")) {
		t.Error("docker: transport error classified as missing")
	}
	if !(appleEngine{}).imageMissing(imageErr("container", "image not found: redis:7-alpine")) {
		t.Error("apple: not-found not classified as missing")
	}
	if (appleEngine{}).imageMissing(imageErr("container", "XPC connection error")) {
		t.Error("apple: transport error classified as missing")
	}
}

func TestParseImageExists(t *testing.T) {
	if (dockerEngine{}).parseImageExists([]byte(`[]`), "") {
		t.Error("docker: empty array means absent")
	}
	if !(dockerEngine{}).parseImageExists([]byte(`[{"Id":"sha256:x"}]`), "") {
		t.Error("docker: non-empty array means present")
	}
	if (appleEngine{}).parseImageExists([]byte(`[]`), "") {
		t.Error("apple: empty array means absent")
	}
	if !(appleEngine{}).parseImageExists([]byte(testImageInspectJSON), "") {
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
	var g flightGroup[struct{}]
	var calls int
	var mu sync.Mutex
	const n = 50

	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = doErr(context.Background(), &g, "key", func() error {
			mu.Lock()
			calls++
			mu.Unlock()
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never entered flight")
	}

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := doErr(context.Background(), &g, "key", func() error { return nil }); err != nil {
				t.Errorf("do: %v", err)
			}
		}()
	}
	// Give waiters a moment to join the in-flight entry before
	// releasing the leader.
	time.Sleep(50 * time.Millisecond)
	close(release)
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
	var g flightGroup[struct{}]
	if err := doErr(context.Background(), &g, "key", func() error { return nil }); err != nil {
		t.Fatalf("first: %v", err)
	}
	executed := false
	if err := doErr(context.Background(), &g, "key", func() error {
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
	var g flightGroup[struct{}]
	first := doErr(context.Background(), &g, "key", func() error {
		return fmt.Errorf("boom")
	})
	if first == nil || first.Error() != "boom" {
		t.Fatalf("first error = %v", first)
	}
	// A completed failed flight must not be reused; the next call
	// executes again.
	second := doErr(context.Background(), &g, "key", func() error { return nil })
	if second != nil {
		t.Fatalf("second error = %v, want nil", second)
	}
}

func TestFlightGroupCancelledWaiterDoesNotAffectLeader(t *testing.T) {
	var g flightGroup[struct{}]
	release := make(chan struct{})
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- doErr(context.Background(), &g, "key", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := doErr(ctx, &g, "key", func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Errorf("leader error = %v", err)
	}
}

func TestFlightGroupCancelledLeaderDoesNotAffectWaiters(t *testing.T) {
	var g flightGroup[struct{}]
	release := make(chan struct{})
	started := make(chan struct{})

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- doErr(leaderCtx, &g, "key", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- doErr(context.Background(), &g, "key", func() error {
			t.Error("waiter must not execute fn")
			return nil
		})
	}()
	// Let the waiter join the in-flight work before cancelling the leader.
	time.Sleep(20 * time.Millisecond)
	leaderCancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader error = %v, want context.Canceled", err)
	}

	close(release)
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Errorf("waiter error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not observe leader completion")
	}
}

func TestFlightKeySeparatesPullAndMissing(t *testing.T) {
	eng := dockerEngine{}
	if flightKey(eng, "redis:7-alpine", flightPull, "") == flightKey(eng, "redis:7-alpine", flightMissing, "") {
		t.Fatal("mandatory pull and missing-check keys must differ")
	}
}

func TestFlightKeySeparatesPlatforms(t *testing.T) {
	eng := dockerEngine{}
	if flightKey(eng, "redis:7-alpine", flightMissing, "linux/amd64") == flightKey(eng, "redis:7-alpine", flightMissing, "linux/arm64") {
		t.Fatal("different platforms must not share a flight")
	}
	if flightKey(eng, "redis:7-alpine", flightMissing, "") == flightKey(eng, "redis:7-alpine", flightMissing, "linux/amd64") {
		t.Fatal("platform and no-platform must not share a flight")
	}
}

// hookRunner invokes before each CLI call, then delegates to fakeRunner.
type hookRunner struct {
	*fakeRunner
	before func(args []string)
}

func (h *hookRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if h.before != nil {
		h.before(args)
	}
	return h.fakeRunner.Run(ctx, args...)
}

func TestEnsureImagePullAlwaysDoesNotJoinMissingFlight(t *testing.T) {
	// A PullMissing flight that finds the image present must not satisfy
	// a concurrent PullAlways; otherwise the mandatory pull is skipped.
	base := newTestRunner()
	base.imagePresent = true

	inspectEntered := make(chan struct{})
	releaseInspect := make(chan struct{})
	var inspectOnce sync.Once
	r := &hookRunner{
		fakeRunner: base,
		before: func(args []string) {
			if args[0] == "image" && len(args) > 1 && args[1] == "inspect" {
				inspectOnce.Do(func() { close(inspectEntered) })
				<-releaseInspect
			}
		},
	}

	missingDone := make(chan error, 1)
	go func() {
		cfg := &config{runner: r, eng: dockerEngine{}, pullPolicy: PullMissing}
		missingDone <- cfg.ensureImage(context.Background(), "redis:7-alpine")
	}()
	<-inspectEntered

	alwaysDone := make(chan error, 1)
	go func() {
		cfg := &config{runner: r, eng: dockerEngine{}, pullPolicy: PullAlways}
		alwaysDone <- cfg.ensureImage(context.Background(), "redis:7-alpine")
	}()

	select {
	case err := <-alwaysDone:
		if err != nil {
			t.Fatalf("PullAlways: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PullAlways blocked behind PullMissing inspect-only flight")
	}

	close(releaseInspect)
	if err := <-missingDone; err != nil {
		t.Fatalf("PullMissing: %v", err)
	}
	if base.pullCalls != 1 {
		t.Errorf("pulls = %d, want 1 from PullAlways", base.pullCalls)
	}
}

func TestPullWithSharesFlightAcrossConcurrentCallers(t *testing.T) {
	base := newTestRunner()
	eng := dockerEngine{}
	release := make(chan struct{})
	releaseStarted := make(chan struct{})
	var once sync.Once
	r := &hookRunner{
		fakeRunner: base,
		before: func(args []string) {
			if args[0] == "pull" {
				once.Do(func() { close(releaseStarted) })
				<-release
			}
		},
	}
	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = pullWith(context.Background(), r, eng, "redis:7-alpine")
		}()
	}
	<-releaseStarted
	// Let all waiters join the in-flight pull before releasing it.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if base.pullCalls != 1 {
		t.Fatalf("pulls = %d, want 1 (shared flight)", base.pullCalls)
	}
}

func TestPullWithClassifiesBackendDown(t *testing.T) {
	f := &fakeRunner{systemUp: false, binary: "docker"}
	if err := pullWith(context.Background(), f, dockerEngine{}, "redis:7-alpine"); !errors.Is(err, ErrSystemNotRunning) {
		t.Fatalf("error = %v, want ErrSystemNotRunning", err)
	}
}

func TestPullWithRejectsInvalidImageBeforeCLICall(t *testing.T) {
	f := newTestRunner()
	if err := Pull(context.Background(), "-bad"); err == nil {
		t.Fatal("want error for invalid image")
	}
	if len(f.calls) != 0 {
		t.Errorf("CLI was called despite invalid image: %v", f.calls)
	}
}

func TestPruneReuseGroupWithFakeRunner(t *testing.T) {
	f := newTestRunner()
	// fakeRunner answers list calls with empty output by default; drive
	// the parse/remove path through a stub runner instead.
	r := &reuseGroupRunner{ids: []string{"a", "b"}}
	removed, err := pruneReuseGroupWith(context.Background(), r, dockerEngine{}, "integration")
	if err != nil {
		t.Fatalf("pruneReuseGroupWith: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v, want 2 ids", removed)
	}
	if r.listCalls != 1 || r.deleteCalls != 2 {
		t.Errorf("list=%d delete=%d, want 1/2", r.listCalls, r.deleteCalls)
	}
	_ = f
}

type reuseGroupRunner struct {
	ids         []string
	listCalls   int
	deleteCalls int
}

func (r *reuseGroupRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "ps":
		r.listCalls++
		// docker parseReuseGroupIDs splits lines; return the stub ids.
		return []byte("a\nb\n"), nil, nil
	case "ls":
		r.listCalls++
		return []byte(`[{"id":"a","configuration":{"labels":{"com.github.hirokazumiyaji.container-go.reuse-group":"integration"}}},{"id":"b","configuration":{"labels":{"com.github.hirokazumiyaji.container-go.reuse-group":"integration"}}}]`), nil, nil
	case "rm", "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}
