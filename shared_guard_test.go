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
// with its own sync pack. The peer is the other process or test holding
// the same name.
func sharedHandle(t *testing.T, r *fakeRunner) *Container {
	t.Helper()
	return &Container{
		id:       "ci-db",
		runner:   r,
		eng:      dockerEngine{},
		reused:   true,
		creation: "aaaaaaaaaaaaaaaa",
		sync:     &lockPack{uid: dockerFixtureID},
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

func TestSharedHandleRefusesStop(t *testing.T) {
	r := newTestRunner()
	ctr := sharedHandle(t, r)

	err := ctr.Stop(context.Background(), nil)
	assertRefused(t, r, err)
	if !strings.Contains(err.Error(), "ci-db") {
		t.Errorf("error = %v, want it to name the container", err)
	}
}

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

	t.Run("copy content to", func(t *testing.T) {
		r := newTestRunner()
		ctr := sharedHandle(t, r)
		assertRefused(t, r, ctr.CopyContentToContainer(context.Background(), []byte("{}"), "/etc/config.json", 0))
	})

	t.Run("copy from", func(t *testing.T) {
		r := newTestRunner()
		ctr := sharedHandle(t, r)
		_, err := ctr.CopyFileFromContainer(context.Background(), "/etc/config.json")
		assertRefused(t, r, err)
	})
}

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
	if !ctr2.reused {
		t.Error("Shared() cleared reused on the original handle")
	}
	if unguarded.reused != true {
		t.Error("Shared() cleared reused on the returned handle; Cleanup would delete peers")
	}
}

func TestSharedHandlePreservesTargetHost(t *testing.T) {
	r := newTestRunner()
	ctr := sharedHandle(t, r)
	ctr.targetHost = "reuse-original"
	unguarded := ctr.Shared()

	t.Setenv("DOCKER_HOST", "tcp://later-host:2375")
	if got := unguarded.TargetHost(); got != ctr.TargetHost() {
		t.Errorf("Shared().TargetHost() = %q, want %q", got, ctr.TargetHost())
	}
	if got := unguarded.TargetHost(); got != "reuse-original" {
		t.Errorf("Shared().TargetHost() = %q, want the original target", got)
	}
}

func TestSharedHandleCopiesShareIdentity(t *testing.T) {
	r := newTestRunner()
	r.inspectJSON = `[{"Id":"` + dockerFixtureID + `","Name":"/ci-db","State":{"Status":"running"},
	  "Config":{"Image":"redis:7-alpine","Labels":{}},
	  "NetworkSettings":{"IPAddress":"","Ports":{},"Networks":{}}}]`
	ctr := sharedHandle(t, r)
	unguarded := ctr.Shared()

	ctx := context.Background()
	if _, err := ctr.cachedInfo(ctx); err != nil {
		t.Fatalf("cachedInfo: %v", err)
	}
	if got := unguarded.immutableID(); got != dockerFixtureID {
		t.Errorf("Shared().immutableID() = %q, want the ID promoted through the original handle", got)
	}
	before := len(r.calls)
	if _, err := unguarded.cachedInfo(ctx); err != nil {
		t.Fatalf("cachedInfo through Shared(): %v", err)
	}
	if got := len(r.calls); got != before {
		t.Errorf("Shared() ran %d extra inspects; the two handles do not share the cache", got-before)
	}
}

func TestSharedHandleNilIsNil(t *testing.T) {
	var ctr *Container
	if got := ctr.Shared(); got != nil {
		t.Errorf("Shared() on nil = %v, want nil", got)
	}
}

func countCallsWith(calls [][]string, subcommand string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == subcommand {
			n++
		}
	}
	return n
}

func TestTerminateContainerStillSkipsSharedHandles(t *testing.T) {
	r := newTestRunner()
	ctr := sharedHandle(t, r)
	unguarded := ctr.Shared()

	if err := TerminateContainer(context.Background(), unguarded); err != nil {
		t.Errorf("TerminateContainer: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("TerminateContainer invoked the runner %v on a shared handle", r.calls)
	}
}

func TestStopUnaffectedOnNonSharedHandles(t *testing.T) {
	r := newTestRunner()
	ctr := &Container{id: "mine", runner: r, eng: dockerEngine{}, sync: &lockPack{uid: dockerFixtureID}}

	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("runner saw %d calls, want 1", len(r.calls))
	}
}

func TestWithFilesRunsBeforeTheHandleIsShared(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(hostFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

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

	callsAfterCreate := len(base.calls)
	if err := ctr.CopyToContainer(context.Background(), hostFile, "/etc/other.json"); !errors.Is(err, ErrSharedContainer) {
		t.Errorf("CopyToContainer after create = %v, want ErrSharedContainer", err)
	}
	if got := len(base.calls); got != callsAfterCreate {
		t.Errorf("refused copy still invoked the runner (%d -> %d)", callsAfterCreate, got)
	}
}
