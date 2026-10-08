package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestReuseAttachCopiesWithFiles(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	call := f.callWith("cp")
	if call == nil || call[1] != src || call[2] != "myctr:/fixture.txt" {
		t.Fatalf("cp call = %v, want attach copy", call)
	}
}

func TestReuseAttachPullAlwaysPulls(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.pullCalls != 1 {
		t.Fatalf("pulls = %d, want 1 for PullAlways attach", f.pullCalls)
	}
}

type reusePullFailRunner struct {
	*attachRunner
}

func (r *reusePullFailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull") {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected pull failure"}
	}
	return r.attachRunner.Run(ctx, args...)
}

func TestReuseAttachPullAlwaysPropagatesFailure(t *testing.T) {
	base := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	base.imagePresent = true
	f := &reusePullFailRunner{attachRunner: base}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "injected pull failure") {
		t.Fatalf("error = %v, want pull failure", err)
	}
	if f.callWith("run") != nil {
		t.Fatal("run issued after PullAlways attach pull failure")
	}
}

type reusePullThenMismatchRunner struct {
	*attachRunner
	pulled atomic.Bool
}

func (r *reusePullThenMismatchRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull") {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		r.pulled.Store(true)
		return nil, nil, nil
	}
	if args[0] == "inspect" {
		image := "redis:7-alpine@sha256:old"
		if r.pulled.Load() {
			image = "redis:7-alpine@sha256:new"
		}
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return []byte(reuseInspectJSON(args[len(args)-1], "running", image)), nil, nil
	}
	return r.attachRunner.Run(ctx, args...)
}

func TestReusePullAlwaysRechecksImageAfterPull(t *testing.T) {
	base := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	base.imagePresent = true
	f := &reusePullThenMismatchRunner{attachRunner: base}

	_, err := Run(context.Background(), "redis:7-alpine@sha256:old",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want post-pull image mismatch", err)
	}
	if f.callWith("delete") != nil {
		t.Fatal("post-pull mismatch must not delete the running shared container")
	}
}

type reuseCopyFailRunner struct {
	*attachRunner
}

func (r *reuseCopyFailRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "cp" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected copy failure"}
	}
	return r.attachRunner.Run(ctx, args...)
}

func TestReuseAttachWithFilesFailureLeavesSharedContainer(t *testing.T) {
	base := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	base.imagePresent = true
	f := &reuseCopyFailRunner{attachRunner: base}
	src := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(src, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithFiles(File{HostPath: src, ContainerPath: "/fixture.txt"}),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "injected copy failure") {
		t.Fatalf("error = %v, want copy failure", err)
	}
	if f.callWith("delete") != nil {
		t.Fatal("attach copy failure must not delete the shared container")
	}
}

type blockingReuseRunner struct {
	*reuseCreateRunner
	runStarted chan struct{}
	releaseRun chan struct{}
	startOnce  sync.Once
}

func newBlockingReuseRunner() *blockingReuseRunner {
	return &blockingReuseRunner{
		reuseCreateRunner: newReuseCreateRunner(),
		runStarted:        make(chan struct{}),
		releaseRun:        make(chan struct{}),
	}
}

func (r *blockingReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "run" {
		r.startOnce.Do(func() { close(r.runStarted) })
		select {
		case <-r.releaseRun:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return r.reuseCreateRunner.Run(ctx, args...)
}

func waitForReuseFlightJoin(t *testing.T) <-chan struct{} {
	t.Helper()
	joined := make(chan struct{})
	var once sync.Once
	old := reuseFlights.onJoin
	reuseFlights.onJoin = func(string) {
		once.Do(func() { close(joined) })
	}
	t.Cleanup(func() { reuseFlights.onJoin = old })
	return joined
}

func countRunnerCalls(r *fakeRunner, subcommand string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if len(call) > 0 && call[0] == subcommand {
			n++
		}
	}
	return n
}

func TestReuseConcurrentLeaderAndWaiterApplyOwnFiles(t *testing.T) {
	f := newBlockingReuseRunner()
	dir := t.TempDir()
	leaderFile := filepath.Join(dir, "leader.txt")
	waiterFile := filepath.Join(dir, "waiter.txt")
	for _, path := range []string{leaderFile, waiterFile} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	joined := waitForReuseFlightJoin(t)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("shared"), WithReuse(), WithFiles(File{HostPath: leaderFile, ContainerPath: "/leader.txt"}),
			withRunner(f), withEngine(appleEngine{}))
		leaderErr <- err
	}()
	<-f.runStarted

	waiterErr := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("shared"), WithReuse(), WithFiles(File{HostPath: waiterFile, ContainerPath: "/waiter.txt"}),
			withRunner(f), withEngine(appleEngine{}))
		waiterErr <- err
	}()
	<-joined
	close(f.releaseRun)

	if err := <-leaderErr; err != nil {
		t.Fatalf("leader: %v", err)
	}
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if got := countRunnerCalls(f.fakeRunner, "cp"); got != 2 {
		t.Fatalf("cp calls = %d, want one copy per caller", got)
	}
}

func TestReuseConcurrentPullAlwaysWaiterPullsBeforeAttach(t *testing.T) {
	f := newBlockingReuseRunner()
	joined := waitForReuseFlightJoin(t)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("shared"), WithReuse(), withRunner(f), withEngine(appleEngine{}))
		leaderErr <- err
	}()
	<-f.runStarted

	waiterErr := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("shared"), WithReuse(), WithPullPolicy(PullAlways),
			withRunner(f), withEngine(appleEngine{}))
		waiterErr <- err
	}()
	<-joined
	close(f.releaseRun)

	if err := <-leaderErr; err != nil {
		t.Fatalf("leader: %v", err)
	}
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if f.pullCalls != 1 {
		t.Fatalf("pulls = %d, want one aggregated PullAlways fetch", f.pullCalls)
	}
}

func TestReuseSequentialPullAlwaysPullsEveryAttach(t *testing.T) {
	f := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	f.imagePresent = true
	for i := range 2 {
		if _, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
			withRunner(f), withEngine(appleEngine{})); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
	}
	if f.pullCalls != 2 {
		t.Fatalf("pulls = %d, want one pull per sequential attach", f.pullCalls)
	}
}

func TestReuseCreatePullAlwaysDoesNotPullTwice(t *testing.T) {
	f := newReuseCreateRunner()
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
		withRunner(f), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.pullCalls != 1 {
		t.Fatalf("pulls = %d, want one fetch for the creating caller", f.pullCalls)
	}
}

type concurrentPullRunner struct {
	*attachRunner
	pullStarted chan struct{}
	releasePull chan struct{}
	startOnce   sync.Once
}

func (r *concurrentPullRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull") {
		r.startOnce.Do(func() {
			close(r.pullStarted)
			<-r.releasePull
		})
	}
	return r.attachRunner.Run(ctx, args...)
}

func TestReuseConcurrentPullAlwaysBothPull(t *testing.T) {
	base := &attachRunner{fakeRunner: newTestRunner(), state: "running"}
	base.imagePresent = true
	r := &concurrentPullRunner{
		attachRunner: base,
		pullStarted:  make(chan struct{}),
		releasePull:  make(chan struct{}),
	}

	err1 := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
			withRunner(r), withEngine(appleEngine{}))
		err1 <- err
	}()
	<-r.pullStarted

	err2 := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "redis:7-alpine",
			WithName("myctr"), WithReuse(), WithPullPolicy(PullAlways),
			withRunner(r), withEngine(appleEngine{}))
		err2 <- err
	}()

	close(r.releasePull)
	if err := <-err1; err != nil {
		t.Fatalf("first caller: %v", err)
	}
	if err := <-err2; err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if r.pullCalls != 2 {
		t.Fatalf("pullCalls = %d, want each PullAlways caller to perform its own pull", r.pullCalls)
	}
}

