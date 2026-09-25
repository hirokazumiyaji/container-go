package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

type reuseGenerationConflictRunner struct{}

func (reuseGenerationConflictRunner) Run(context.Context, ...string) ([]byte, []byte, error) {
	return nil, nil, &cli.CLIError{
		Args:     []string{"run"},
		ExitCode: 1,
		Stderr:   `Error: already exists: container "reuse-generation"`,
	}
}

func TestReuseCreateAllocatesFreshGenerationOnEveryRetry(t *testing.T) {
	cfg := &config{
		runner:   reuseGenerationConflictRunner{},
		eng:      appleEngine{},
		name:     "reuse-generation",
		creation: "aaaaaaaaaaaaaaaa",
	}
	image := imageIdentity{reference: "redis:7-alpine", pinned: true}
	if _, err := reuseCreateResolved(context.Background(), "redis:7-alpine", cfg, image); err == nil {
		t.Fatal("first conflicting create unexpectedly succeeded")
	}
	first := cfg.creation
	if first == "aaaaaaaaaaaaaaaa" || !validCreationGeneration(first) {
		t.Fatalf("first retry generation = %q", first)
	}
	if _, err := reuseCreateResolved(context.Background(), "redis:7-alpine", cfg, image); err == nil {
		t.Fatal("second conflicting create unexpectedly succeeded")
	}
	if cfg.creation == first || !validCreationGeneration(cfg.creation) {
		t.Fatalf("second retry generation = %q, first = %q", cfg.creation, first)
	}
}

type reuseHandoffExternalRunner struct {
	binary   string
	uid      string
	creation string
}

func (r *reuseHandoffExternalRunner) External() bool         { return true }
func (r *reuseHandoffExternalRunner) ExternalBinary() string { return r.binary }

func (r *reuseHandoffExternalRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		return []byte(`[{"Id":"sha256:` + strings.Repeat("c", 64) + `","RepoDigests":["docker.io/library/redis@sha256:` + strings.Repeat("b", 64) + `"]}]`), nil, nil
	case args[0] == "run":
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if creation, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = creation
				}
			}
		}
		return []byte(r.uid + "\n"), nil, nil
	case args[0] == "inspect":
		if args[len(args)-1] != r.uid {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "No such container"}
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"Created":"2026-08-19T01:23:45Z","Image":"sha256:%s","State":{"Status":"running"},"Config":{"Image":"docker.io/library/redis@sha256:%s","Labels":{%q:"true",%q:"true",%q:%q}},"NetworkSettings":{"IPAddress":"172.17.0.2","Ports":{}}}]`, r.uid, strings.Repeat("c", 64), strings.Repeat("b", 64), managedLabel, reuseLabel, creationLabel, r.creation)), nil, nil
	default:
		return nil, nil, nil
	}
}

type keepPostCreateRunner struct {
	*fakeRunner
}

func (r *keepPostCreateRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "injected post-create inspect failure"}
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestKeepPreventsRollbackDeletionAndReturnsHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	f := &keepPostCreateRunner{fakeRunner: newTestRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-post-create"), withRunner(f), withEngine(appleEngine{}))
	if err == nil || ctr == nil {
		t.Fatalf("Run = (%v, %v), want retained handle and error", ctr, err)
	}
	if len(f.calls) == 0 {
		t.Fatal("runner was not called")
	}
	for _, call := range f.calls {
		if len(call) > 0 && (call[0] == "delete" || call[0] == "rm") {
			t.Fatalf("KEEP issued deletion: %v", call)
		}
	}
}

type unverifiedReuseRunner struct {
	*fakeRunner
	created bool
}

func (r *unverifiedReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "run" {
		r.created = true
	}
	if len(args) > 0 && args[0] == "inspect" {
		if !r.created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "No such container"}
		}
		creation := r.creations[args[len(args)-1]]
		return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine"},"labels":{%q:"true",%q:"true",%q:%q}},"status":{"state":"created","networks":[]}}]`, args[len(args)-1], args[len(args)-1], managedLabel, reuseLabel, creationLabel, creation)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestKeepNeverReturnsUnverifiedReuseHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	base := newTestRunner()
	runner := &unverifiedReuseRunner{fakeRunner: base}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("unverified-reuse"), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if err == nil || ctr != nil {
		t.Fatalf("Run = (%v, %v), want nil handle and verification error", ctr, err)
	}
	for _, call := range base.calls {
		if len(call) > 0 && (call[0] == "delete" || call[0] == "rm") {
			t.Fatalf("unverified reusable create was deleted: %v", call)
		}
	}
	ctr, err = reuseFailureResult(&Container{reused: true}, context.Canceled)
	if ctr != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("reuseFailureResult = (%v, %v), want nil handle and cause", ctr, err)
	}
}

func TestKeepPreventsStoppedReuseRecycling(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	r := &generationStateRunner{creation: "0123456789abcdef", state: "stopped"}
	cfg := &config{runner: r, eng: appleEngine{}, name: "keep-stopped"}
	if err := deleteStoppedReuse(context.Background(), cfg, &engineInfo{
		state: StateStopped,
		labels: map[string]string{
			managedLabel: "true", reuseLabel: "true", creationLabel: "0123456789abcdef",
		},
	}); err == nil {
		t.Fatal("deleteStoppedReuse unexpectedly succeeded under KEEP")
	}
	if r.deleteCalls != 0 {
		t.Fatalf("KEEP issued stopped-reuse deletion: %d", r.deleteCalls)
	}
}

func TestReuseCreateDoesNotRegisterReaper(t *testing.T) {
	binary := "/tmp/container-go-reuse-no-reaper-test-binary"
	globalReapersMu.Lock()
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	runner := &reuseHandoffExternalRunner{binary: binary, uid: strings.Repeat("b", 64)}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-no-reaper"), WithReuse(), withRunner(runner), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	globalReapersMu.Lock()
	_, exists := globalReapers[binary]
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	if exists {
		t.Fatal("reusable create registered a watchdog entry")
	}
}

func TestReuseHandoffDoesNotLeaveReaperOwnership(t *testing.T) {
	binary := "/tmp/container-go-reuse-handoff-test-binary"
	uid := strings.Repeat("a", 64)
	globalReapersMu.Lock()
	delete(globalReapers, binary)
	globalReapersMu.Unlock()

	// Simulate a normal run's still-active watchdog entry for the same
	// logical name. A successful reuse handoff must remove it too.
	watchdog := newReaper(binary, "rm")
	if err := watchdog.register("reuse-handoff", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := watchdog.register(uid, ""); err != nil {
		t.Fatal(err)
	}
	globalReapersMu.Lock()
	globalReapers[binary] = watchdog
	globalReapersMu.Unlock()
	defer watchdog.closeStdin()

	runner := &reuseHandoffExternalRunner{binary: binary, uid: uid}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-handoff"), WithReuse(), withRunner(runner), withEngine(dockerEngine{})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	globalReapersMu.Lock()
	r := globalReapers[binary]
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	if r != nil {
		r.mu.Lock()
		entries := len(r.entries)
		activeChild := r.cmd != nil
		r.mu.Unlock()
		if entries != 0 || activeChild {
			t.Fatalf("reaper retained %d entries/active=%v after handoff", entries, activeChild)
		}
	}
}
