package container

import (
	"context"
	"sync"
	"testing"
	"time"
)

// slowInspectRunner answers inspect only after the gate is released, and
// records how many inspect calls it received. It stands in for a
// backend that takes seconds to answer.
type slowInspectRunner struct {
	*fakeRunner

	release chan struct{}
	arrived chan struct{}
	once    sync.Once

	mu    sync.Mutex
	calls int
}

func (r *slowInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()
		r.once.Do(func() { close(r.arrived) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return []byte(`[{
  "Id": "aaaa",
  "Name": "/c1",
  "State": {"Status": "running"},
  "Config": {"Image": "redis:7-alpine", "Labels": {}},
  "NetworkSettings": {
    "IPAddress": "",
    "Ports": {"80/tcp": [{"HostIp": "0.0.0.0", "HostPort": "49154"}]},
    "Networks": {"bridge": {"IPAddress": "172.17.0.2"}}
  }
}]`), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func (r *slowInspectRunner) inspectCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newSlowInspectRunner() *slowInspectRunner {
	return &slowInspectRunner{
		fakeRunner: newTestRunner(),
		release:    make(chan struct{}),
		arrived:    make(chan struct{}),
	}
}

// TestCachedInfoRunsTheInspectOutsideTheCacheLock pins the point of
// moving the inspect out of c.mu. The inspect is a subprocess bounded
// by queryTimeout, so holding the cache lock across it would block
// every other cache reader for that whole window.
//
// This is a white-box assertion on the lock rather than on observable
// behaviour: with the inspect under c.mu, concurrent cachedInfo callers
// are still collapsed to one inspect (the first one populates the
// cache, the rest read it), so counting subprocesses cannot tell the
// two versions apart. Acquiring c.mu from the test while the inspect is
// held open can.
func TestCachedInfoRunsTheInspectOutsideTheCacheLock(t *testing.T) {
	runner := newSlowInspectRunner()
	ctr := &Container{id: "c1", runner: runner, eng: dockerEngine{}}

	done := make(chan error, 1)
	go func() {
		_, err := ctr.cachedInfo(context.Background())
		done <- err
	}()

	// Wait until the inspect is in flight.
	select {
	case <-runner.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("inspect never started")
	}

	// TryLock rather than Lock: a blocking Lock would deadlock the test
	// against the inspect we are deliberately holding open.
	if !ctr.mu.TryLock() {
		t.Error("c.mu is still held while the inspect runs; the subprocess is inside the cache lock")
	} else {
		ctr.mu.Unlock()
	}

	close(runner.release)
	if err := <-done; err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
}

// TestCachedInfoCollapsesConcurrentCallersOntoOneInspect keeps the
// subprocess count honest after the lock moved: the singleflight, not
// the mutex, is what makes N concurrent callers cost one inspect.
func TestCachedInfoCollapsesConcurrentCallersOntoOneInspect(t *testing.T) {
	runner := newSlowInspectRunner()
	ctr := &Container{id: "c1", runner: runner, eng: dockerEngine{}}

	ctx := context.Background()
	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			_, errs[i] = ctr.cachedInfo(ctx)
		}()
	}

	// Wait until the single inspect is in flight, then give the other
	// callers time to arrive at the flight. If the inspect ran under
	// c.mu, these would be blocked on the mutex rather than joined onto
	// the flight; either way they must not each start their own.
	select {
	case <-runner.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("inspect never started")
	}
	time.Sleep(100 * time.Millisecond)

	if got := runner.inspectCalls(); got != 1 {
		t.Errorf("inspect ran %d times while the first was still in flight, want 1", got)
	}

	close(runner.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: cachedInfo: %v", i, err)
		}
	}
	if got := runner.inspectCalls(); got != 1 {
		t.Errorf("inspect ran %d times for %d concurrent callers, want 1", got, callers)
	}
	if got := ctr.immutableID(); got != "aaaa" {
		t.Errorf("immutableID = %q, want the ID from the shared inspect", got)
	}
}

// TestCachedInfoCancellationDoesNotBlockOtherCallers keeps a cancelled
// waiter from stalling the ones behind it. The flight returns the
// waiter's context error without disturbing the execution.
func TestCachedInfoCancellationDoesNotBlockOtherCallers(t *testing.T) {
	runner := newSlowInspectRunner()
	ctr := &Container{id: "c1", runner: runner, eng: dockerEngine{}}

	// The first caller starts the inspect and then gives up on it.
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := ctr.cachedInfo(firstCtx)
		firstDone <- err
	}()
	select {
	case <-runner.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("inspect never started")
	}
	cancelFirst()
	if err := <-firstDone; err == nil {
		t.Error("want the cancelled caller's context error")
	}

	// A caller arriving afterwards still gets the result once the
	// inspect lands.
	second := make(chan error, 1)
	go func() {
		_, err := ctr.cachedInfo(context.Background())
		second <- err
	}()
	close(runner.release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second cachedInfo: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second caller never completed")
	}
	if got := ctr.immutableID(); got != "aaaa" {
		t.Errorf("immutableID = %q, want the ID from the inspect", got)
	}
	// The point: the cancelled leader must not have cost the waiters a
	// second inspect. If the flight ran on the leader's context, that
	// cancellation would have aborted it and the second caller would
	// have had to start its own.
	if got := runner.inspectCalls(); got != 1 {
		t.Errorf("inspect ran %d times, want 1: the cancelled leader's context reached the shared inspect", got)
	}
}
