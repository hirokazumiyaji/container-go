package container

import (
	"context"
	"sync"
	"testing"
)

// uidReportingRunner answers inspect with a backend-assigned immutable
// ID, and records how many inspect calls it served.
type uidReportingRunner struct {
	*fakeRunner

	mu    sync.Mutex
	calls int
}

func (r *uidReportingRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()
		return []byte(cachedInfoInspectJSON), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *uidReportingRunner) inspectCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestContainerUIDIsRaceFreeAcrossEndpointAndTerminate is the
// regression for the data race on Container.uid. The write happens on
// the inspect path (promoted from the first inspect) and the read on
// the Terminate path, so any code that calls Endpoint concurrently with
// Terminate - a readiness wait racing a teardown - is undefined
// behaviour without synchronization.
//
// The published-port path is the one that reaches the inspect:
// resolve returns a declared publish spec without inspecting, so the
// handle is built with only WithExposedPorts.
func TestContainerUIDIsRaceFreeAcrossEndpointAndTerminate(t *testing.T) {
	runner := &uidReportingRunner{fakeRunner: newTestRunner()}
	ctr := &Container{
		id:      "c1",
		runner:  runner,
		eng:     dockerEngine{},
		exposed: []portSpec{{port: 80, proto: "tcp"}},
	}

	ctx := context.Background()
	if _, err := ctr.Endpoint(ctx, "80"); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	// The first Endpoint already inspected, so the ID is promoted before
	// the concurrent phase. That is fine for the race: what matters is
	// that the write and the reads are both synchronized.
	if got := ctr.immutableID(); got != "aaaa" {
		t.Fatalf("immutableID = %q, want the ID reported by the first inspect", got)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			ctr.Endpoint(ctx, "80")
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			ctr.Terminate(ctx)
		}
	}()
	wg.Wait()

	if got := ctr.immutableID(); got == "" {
		t.Error("immutableID is still empty; the promote never happened")
	}
}

// TestContainerImmutableIDNeverDowngrades covers the other half of the
// invariant: once promoted, a later inspect that reports an empty ID
// must not clear the value, or a concurrent promote could race a
// reader into deleting by name instead of by immutable ID.
func TestContainerImmutableIDNeverDowngrades(t *testing.T) {
	ctr := &Container{id: "c1", runner: newTestRunner(), eng: dockerEngine{}}
	ctr.setImmutableID("aaaa")
	ctr.setImmutableID("")
	ctr.setImmutableID("bbbb")
	if got := ctr.immutableID(); got != "aaaa" {
		t.Errorf("immutableID = %q, want the first promoted value %q", got, "aaaa")
	}
}

// TestCachedInfoReusesTheResultAfterTheFirstInspect keeps the cache
// itself intact: a second call must not re-inspect.
func TestCachedInfoReusesTheResultAfterTheFirstInspect(t *testing.T) {
	runner := &uidReportingRunner{fakeRunner: newTestRunner()}
	ctr := &Container{id: "c1", runner: runner, eng: dockerEngine{}}

	ctx := context.Background()
	for range 5 {
		if _, err := ctr.cachedInfo(ctx); err != nil {
			t.Fatalf("cachedInfo: %v", err)
		}
	}
	if got := runner.inspectCalls(); got != 1 {
		t.Errorf("inspect ran %d times, want 1", got)
	}
}
