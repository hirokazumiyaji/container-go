package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedHandle builds the shape a WithReuse attach returns: reused set,
// with its own state. The peer is the other process or test holding the
// same name.
func sharedHandle(t *testing.T, r *fakeRunner) *Container {
	t.Helper()
	return &Container{
		id:       "ci-db",
		runner:   r,
		eng:      dockerEngine{},
		reused:   true,
		creation: "aaaaaaaaaaaaaaaa",
		state:    &containerState{},
	}
}

// assertRefused checks that err is the shared-container refusal, and that
// the runner was never invoked: the guard exists to avoid the side
// effect, not to report it after the fact.
func assertRefused(t *testing.T, r *fakeRunner, err error) {
	t.Helper()
	if !errors.Is(err, ErrSharedContainer) {
		t.Fatalf("error = %v, want ErrSharedContainer", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("runner was invoked %v; the guard must run before any side effect", r.calls)
	}
}

// TestSharedHandleRefusesStop is the core of the issue: Stop never
// looked at the reused flag, so one process's Stop took down a container
// that other processes were still using, and the damage showed up on
// their side as an unrelated connection failure.
func TestSharedHandleRefusesStop(t *testing.T) {
	r := newTestRunner()
	ctr := sharedHandle(t, r)

	err := ctr.Stop(context.Background(), nil)
	assertRefused(t, r, err)
	if !strings.Contains(err.Error(), "ci-db") {
		t.Errorf("error = %v, want it to name the container", err)
	}
}

// TestSharedHandleRefusesCopies covers the other two destructive
// operations. Copy-to overwrites a file the peers read; copy-out races
// their writes to it.
func TestSharedHandleRefusesCopies(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(hostFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("copy to", func(t *testing.T) {
		r := newTestRunner()
		ctr := sharedHandle(t, r)
		assertRefused(t, r, ctr.CopyToContainer(context.Background(), hostFile, "/etc/config.json"))
	})

	t.Run("copy from", func(t *testing.T) {
		r := newTestRunner()
		ctr := sharedHandle(t, r)
		_, err := ctr.CopyFileFromContainer(context.Background(), "/etc/config.json")
		assertRefused(t, r, err)
	})
}

// TestSharedHandleAllowsReadOnlyOperations keeps the guard from being
// broader than intended: the operations a peer depends on must still
// work, or sharing would be useless.
func TestSharedHandleAllowsReadOnlyOperations(t *testing.T) {
	r := newTestRunner()
	r.imagePresent = true
	r.inspectJSON = `[{"Id":"aaaa","Name":"/ci-db","State":{"Status":"running"},
	  "Config":{"Image":"redis:7-alpine","Labels":{}},
	  "NetworkSettings":{"IPAddress":"","Ports":{"6379/tcp":[{"HostIp":"0.0.0.0","HostPort":"49154"}]},
	    "Networks":{"bridge":{"IPAddress":"172.17.0.2"}}}}]`
	// Declared, not published: this is the path that reaches cachedInfo.
	ctr := sharedHandle(t, r)
	ctr.exposed = []portSpec{{port: 6379, proto: "tcp"}}

	ctx := context.Background()
	if _, err := ctr.Endpoint(ctx, "6379/tcp"); err != nil {
		t.Errorf("Endpoint: %v", err)
	}
	if _, err := ctr.ContainerIP(ctx); err != nil {
		t.Errorf("ContainerIP: %v", err)
	}
	if state, err := ctr.State(ctx); err != nil {
		t.Errorf("State: %v", err)
	} else if state != StateRunning {
		t.Errorf("State = %q, want running", state)
	}
	// Exec runs a command in the container; the guard is about changing
	// what a peer sees, and an explicit exec is the caller's own
	// business.
	if _, _, err := ctr.Exec(ctx, []string{"true"}); err != nil {
		t.Errorf("Exec: %v", err)
	}
}

// TestSharedHandleLiftsTheGuardForItsOwnCopy pins that Shared opts in:
// the returned handle performs the operation, while the original keeps
// its guard.
func TestSharedHandleLiftsTheGuardForItsOwnCopy(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(hostFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := newTestRunner()
	ctr := sharedHandle(t, r)
	unguarded := ctr.Shared()

	if err := unguarded.Stop(context.Background(), nil); err != nil {
		t.Errorf("Shared().Stop: %v", err)
	}
	if err := unguarded.CopyToContainer(context.Background(), hostFile, "/etc/config.json"); err != nil {
		t.Errorf("Shared().CopyToContainer: %v", err)
	}
	if got := len(r.calls); got != 2 {
		t.Errorf("runner saw %d calls, want 2 (stop and copy)", got)
	}

	// The original handle keeps its guard: opting in is per-handle, not a
	// permanent demotion of this container.
	r2 := newTestRunner()
	ctr2 := sharedHandle(t, r2)
	_ = ctr2.Shared()
	if err := ctr2.Stop(context.Background(), nil); !errors.Is(err, ErrSharedContainer) {
		t.Errorf("original handle Stop = %v, want ErrSharedContainer", err)
	}
}

// TestSharedHandleCopiesShareIdentity keeps the two handles on one
// container: the relaxed handle must see the same immutable ID and the
// same inspect cache, not a copy of them.
func TestSharedHandleCopiesShareIdentity(t *testing.T) {
	r := newTestRunner()
	r.inspectJSON = `[{"Id":"aaaa","Name":"/ci-db","State":{"Status":"running"},
	  "Config":{"Image":"redis:7-alpine","Labels":{}},
	  "NetworkSettings":{"IPAddress":"","Ports":{},"Networks":{}}}]`
	ctr := sharedHandle(t, r)
	unguarded := ctr.Shared()

	ctx := context.Background()
	if _, err := ctr.cachedInfo(ctx); err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
	if got := unguarded.immutableID(); got != "aaaa" {
		t.Errorf("Shared().immutableID() = %q, want the ID promoted through the original handle", got)
	}
	// A second cachedInfo through the relaxed handle must reuse the
	// cache rather than inspect again.
	before := len(r.calls)
	if _, err := unguarded.cachedInfo(ctx); err != nil {
		t.Fatalf("cachedInfo through Shared(): %v", err)
	}
	if got := len(r.calls); got != before {
		t.Errorf("Shared() ran %d extra inspects; the two handles do not share the cache", got-before)
	}
}

// TestSharedHandleNilIsNil keeps the nil-receiver behavior consistent
// with the other nil-tolerant helpers, so deferring a Shared() alongside
// an error check is safe.
func TestSharedHandleNilIsNil(t *testing.T) {
	var ctr *Container
	if got := ctr.Shared(); got != nil {
		t.Errorf("Shared() on nil = %v, want nil", got)
	}
}

// countCallsWith counts the recorded CLI invocations of one subcommand.
// callWith only returns the first match, which is not enough when the
// test needs to assert an exact count.
func countCallsWith(calls [][]string, subcommand string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == subcommand {
			n++
		}
	}
	return n
}

// TestTerminateContainerStillSkipsSharedHandles is the pre-existing
// contract: lifting the operation guard must not make teardown start
// deleting containers the peer is using.
func TestTerminateContainerStillSkipsSharedHandles(t *testing.T) {
	r := newTestRunner()
	ctr := sharedHandle(t, r)
	_ = ctr.Shared()

	if err := TerminateContainer(ctr); err != nil {
		t.Errorf("TerminateContainer: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("TerminateContainer invoked the runner %v on a shared handle", r.calls)
	}
}

// TestStopUnaffectedOnNonSharedHandles keeps the guard from leaking into
// ordinary containers.
func TestStopUnaffectedOnNonSharedHandles(t *testing.T) {
	r := newTestRunner()
	ctr := &Container{id: "mine", runner: r, eng: dockerEngine{}, state: &containerState{}}

	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("runner saw %d calls, want 1", len(r.calls))
	}
}

// TestWithFilesRunsBeforeTheHandleIsShared documents why the create path
// uses the unguarded copy: WithFiles is applied while this process is
// creating the container, before any peer can attach, so the shared
// guard does not apply to it.
func TestWithFilesRunsBeforeTheHandleIsShared(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(hostFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The create path: the fixture reports not-found until run succeeds,
	// so this Run creates the container rather than attaching.
	base := newReuseCreateRunner()
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithFiles(File{HostPath: hostFile, ContainerPath: "/etc/config.json"}),
		withRunner(base), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ctr.reused {
		t.Error("handle is not marked reused; WithReuse did not take effect")
	}
	if got := countCallsWith(base.calls, "cp"); got != 1 {
		t.Errorf("cp ran %d times during create, want 1: WithFiles must copy before the handle is shared", got)
	}

	// The copy already happened during Run, so the handle's own copy is
	// refused.
	callsAfterCreate := len(base.calls)
	if err := ctr.CopyToContainer(context.Background(), hostFile, "/etc/other.json"); !errors.Is(err, ErrSharedContainer) {
		t.Errorf("CopyToContainer after create = %v, want ErrSharedContainer", err)
	}
	if got := len(base.calls); got != callsAfterCreate {
		t.Errorf("refused copy still invoked the runner (%d -> %d)", callsAfterCreate, got)
	}
}
