package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// externalRunner makes the fake CLI look like an external backend so the
// Run/reuse paths perform their full ownership verification.
type externalRunner struct {
	*fakeRunner
	bin string
}

func (r *externalRunner) External() bool         { return true }
func (r *externalRunner) ExternalBinary() string { return r.bin }

type externalReuseCreateRunner struct {
	*reuseCreateRunner
	bin string
}

func (r *externalReuseCreateRunner) External() bool         { return true }
func (r *externalReuseCreateRunner) ExternalBinary() string { return r.bin }

// newExternalTestBinary writes a stand-in backend executable so the
// watchdog reaper can be registered without running a real container CLI.
func newExternalTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// cleanupExternalReapers drops the process-wide reaper records a test's
// stand-in backend created so they cannot outlive the test.
func cleanupExternalReapers(t *testing.T, bin string) {
	t.Helper()
	t.Cleanup(func() {
		var stale []*reaper
		globalReapersMu.Lock()
		for key, r := range globalReapers {
			if key == bin || strings.HasPrefix(key, bin+"\x00") {
				delete(globalReapers, key)
				stale = append(stale, r)
			}
		}
		globalReapersMu.Unlock()
		for _, r := range stale {
			r.closeStdin()
		}
	})
}

func containerInspectCalls(r *fakeRunner) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := 0
	for _, args := range r.calls {
		if len(args) > 0 && args[0] == "inspect" {
			calls++
		}
	}
	return calls
}

func TestRunReusesOwnershipInspectForAppleImageVerification(t *testing.T) {
	base := newTestRunner()
	base.imagePresent = true
	bin := newExternalTestBinary(t)
	cleanupExternalReapers(t, bin)
	runner := &externalRunner{fakeRunner: base, bin: bin}

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("perf-run"), withRunner(runner), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := containerInspectCalls(base); got != 1 {
		t.Fatalf("container inspect calls = %d, want 1: the ownership inspect should feed Apple image verification", got)
	}
}

func TestReuseRunReusesVerifiedCreateInspect(t *testing.T) {
	base := newReuseCreateRunner()
	bin := newExternalTestBinary(t)
	cleanupExternalReapers(t, bin)
	runner := &externalReuseCreateRunner{reuseCreateRunner: base, bin: bin}

	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("perf-reuse"), WithReuse(),
		withRunner(runner), withEngine(appleEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Inspects: initial name lookup (not found), ownership verification,
	// per-caller compatibility read, and the post-wait lifecycle check.
	// The created generation's ownership and image checks reuse one result.
	if got := containerInspectCalls(base.fakeRunner); got != 4 {
		t.Fatalf("container inspect calls = %d, want 4: created-generation inspects should be reused", got)
	}
}

func TestVerifiedOperationTargetDoesNotSerializeBehindInspect(t *testing.T) {
	base := newTestRunner()
	started := make(chan struct{})
	release := make(chan struct{})
	var inspectN atomic.Int32
	runner := &blockingDynamicInspectRunner{
		fakeRunner: base,
		started:    started,
		release:    release,
		count:      &inspectN,
	}
	ctr := testDockerContainer(base)
	ctr.runner = runner

	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	inspectDone := make(chan struct{})
	go func() {
		defer close(inspectDone)
		_, _ = ctr.inspectFresh(context.Background())
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	target, err := ctr.verifiedOperationTarget(ctx)
	if err != nil {
		t.Fatalf("verifiedOperationTarget waited for an in-flight inspect: %v", err)
	}
	if target != dockerFixtureID {
		t.Fatalf("verifiedOperationTarget = %q, want immutable ID %q", target, dockerFixtureID)
	}

	unblock()
	<-inspectDone
	if got := inspectN.Load(); got != 1 {
		t.Fatalf("inspect calls = %d, want 1", got)
	}
}

func TestNextWaitExitPollIntervalBacksOffToCap(t *testing.T) {
	want := []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second,
		time.Second,
	}
	current := waitExitPollInterval
	for i, expected := range want {
		current = nextWaitExitPollInterval(current)
		if current != expected {
			t.Fatalf("interval %d = %v, want %v", i, current, expected)
		}
	}
}

func TestWaitBacksOffLifecyclePolling(t *testing.T) {
	oldInterval, oldMax := waitExitPollInterval, waitExitPollMaxInterval
	waitExitPollInterval, waitExitPollMaxInterval = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() {
		waitExitPollInterval, waitExitPollMaxInterval = oldInterval, oldMax
	})

	var inspects atomic.Int32
	runner := waitRunnerFunc(func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "inspect" {
			inspects.Add(1)
			return []byte(dockerInspectWithPort("49153")), nil, nil
		}
		return nil, nil, nil
	})
	ctr := testDockerContainer(newTestRunner())
	ctr.runner = runner

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := ctr.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want context.DeadlineExceeded", err)
	}
	if got := inspects.Load(); got < 2 || got >= 10 {
		t.Fatalf("inspect calls = %d, want a backed-off poll count below the fixed 10ms cadence", got)
	}
}

func dockerInspectHostNetwork() string {
	return fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"HostConfig": {"NetworkMode": "host"},
		"NetworkSettings": {
			"IPAddress": "",
			"Ports": {},
			"Networks": {"host": {"IPAddress": ""}}
		}
	}]`, dockerFixtureID)
}

func dockerInspectBridgeNoIP() string {
	return fmt.Sprintf(`[{
		"Id": %q,
		"Name": "/myctr",
		"State": {"Status": "running"},
		"Config": {"Image": "redis:7-alpine", "Labels": {}},
		"HostConfig": {"NetworkMode": "bridge"},
		"NetworkSettings": {
			"IPAddress": "",
			"Ports": {},
			"Networks": {"bridge": {"IPAddress": ""}}
		}
	}]`, dockerFixtureID)
}

func TestHostNetworkCachesRunningDynamicSnapshot(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectHostNetwork(), dockerInspectHostNetwork()}
	ctr := testDockerContainer(runner)
	ctr.network = dockerNetworkHost
	ctr.networkExplicit = true
	clock := installDynamicClock(ctr)

	first, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("first Host: %v", err)
	}
	clock.Advance(dynamicInspectTTL / 2)
	second, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("second Host: %v", err)
	}
	if first != second {
		t.Fatalf("Host values = %q then %q, want cached host-network result", first, second)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1 within the dynamic TTL for host networking", runner.inspectCalls)
	}
}

func TestPortlessBridgeCachesRunningDynamicSnapshot(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectBridgeNoIP(), dockerInspectBridgeNoIP()}
	ctr := testDockerContainer(runner)
	ctr.network = dockerNetworkBridge
	ctr.networkExplicit = true
	clock := installDynamicClock(ctr)

	first, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("first Host: %v", err)
	}
	clock.Advance(dynamicInspectTTL / 2)
	second, err := ctr.Host(context.Background())
	if err != nil {
		t.Fatalf("second Host: %v", err)
	}
	if first != second {
		t.Fatalf("Host values = %q then %q, want cached port-less result", first, second)
	}
	if runner.inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1 within the dynamic TTL for a port-less bridge container", runner.inspectCalls)
	}
}

func TestContainerIPClearsEmptyPortlessSnapshot(t *testing.T) {
	runner := newTestRunner()
	runner.inspectResponses = []string{dockerInspectBridgeNoIP(), dockerInspectBridgeNoIP()}
	ctr := testDockerContainer(runner)
	ctr.network = dockerNetworkBridge
	ctr.networkExplicit = true

	for call := 1; call <= 2; call++ {
		if _, err := ctr.ContainerIP(context.Background()); err == nil {
			t.Fatalf("ContainerIP call %d succeeded without an IP", call)
		}
		if runner.inspectCalls != call {
			t.Fatalf("inspect calls after ContainerIP call %d = %d, want %d: an empty-IP snapshot must not satisfy the next read", call, runner.inspectCalls, call)
		}
	}
}
