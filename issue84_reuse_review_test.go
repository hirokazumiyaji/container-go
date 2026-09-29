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

func TestKeepNeverReturnsUnverifiedPostCreateHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	f := &keepPostCreateRunner{fakeRunner: newTestRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-post-create"), withRunner(f), withEngine(appleEngine{}))
	if err == nil || ctr != nil {
		t.Fatalf("Run = (%v, %v), want nil handle and joined verification error", ctr, err)
	}
	if !strings.Contains(err.Error(), "injected post-create inspect failure") ||
		!strings.Contains(err.Error(), "verify retained container") {
		t.Fatalf("error = %v, want original and verification errors", err)
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

// keepPlatformRunner reports a complete but incompatible container platform
// for every post-create inspect, so platform resolution fails after create.
type keepPlatformRunner struct {
	*fakeRunner
}

func (r *keepPlatformRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		id := args[len(args)-1]
		creation := r.creations[id]
		if creation == "" {
			creation = r.lastCreation
		}
		return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"id":%q,"image":{"reference":"redis:7-alpine","descriptor":{"digest":"sha256:%s"}},"platform":{"os":"linux","architecture":"arm64"},"labels":{%q:"true",%q:"true",%q:%q}},"status":{"state":"running","networks":[]}}]`,
			id, id, strings.Repeat("b", 64), managedLabel, reuseLabel, creationLabel, creation)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

// TestKeepNeverReturnsUnverifiedPostCreatePlatformHandle covers the platform
// half of the same rule: a constructed handle whose post-create platform
// check failed is not returned under KEEP.
func TestKeepNeverReturnsUnverifiedPostCreatePlatformHandle(t *testing.T) {
	t.Setenv("CONTAINERGO_KEEP", "1")
	f := &keepPlatformRunner{fakeRunner: newTestRunner()}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("keep-post-platform"), WithPlatform("linux/amd64"),
		withRunner(f), withEngine(appleEngine{}))
	if err == nil || ctr != nil {
		t.Fatalf("Run = (%v, %v), want nil handle and joined verification error", ctr, err)
	}
	if !strings.Contains(err.Error(), "platform") ||
		!strings.Contains(err.Error(), "verify retained container") {
		t.Fatalf("error = %v, want platform and verification errors", err)
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
	binary := t.TempDir() + "/docker"
	uid := strings.Repeat("a", 64)
	foreign := "0123456789abcdef"
	globalReapersMu.Lock()
	delete(globalReapers, binary)
	globalReapersMu.Unlock()

	// Simulate a normal run's still-active watchdog entry for a different
	// generation of the same logical name, plus the immutable ID record a
	// create promotion leaves behind. A successful reuse handoff retires its
	// own targets and must not claim the foreign generation.
	watchdog := newReaper(binary, "rm")
	t.Cleanup(watchdog.closeStdin)
	if err := watchdog.register("reuse-handoff", foreign); err != nil {
		t.Fatal(err)
	}
	if err := watchdog.register(uid, ""); err != nil {
		t.Fatal(err)
	}
	globalReapersMu.Lock()
	globalReapers[binary] = watchdog
	globalReapersMu.Unlock()

	runner := &reuseHandoffExternalRunner{binary: binary, uid: uid}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("reuse-handoff"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run returned, so the reuse flight that writes the generation has
	// already been joined.
	created := runner.creation

	globalReapersMu.Lock()
	r := globalReapers[binary]
	delete(globalReapers, binary)
	globalReapersMu.Unlock()
	if r == nil {
		t.Fatal("handoff lost the reaper")
	}
	r.mu.Lock()
	entries := append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || entries[0].id != "reuse-handoff" || entries[0].creation != foreign {
		t.Fatalf("reaper entries = %+v, want only the foreign generation %q", entries, foreign)
	}

	// The generation this handoff owns is matched by name plus creation, and
	// the promoted immutable ID by UID. Neither key can reach a concurrent
	// generation of the same name.
	if err := r.register("reuse-handoff", created); err != nil {
		t.Fatal(err)
	}
	if err := r.unregisterHandoff(ctr.ID(), created, ctr.uid); err != nil {
		t.Fatalf("retire owned generation: %v", err)
	}
	r.mu.Lock()
	entries = append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 1 || entries[0].creation != foreign {
		t.Fatalf("reaper entries = %+v, want only the untouched foreign generation %q", entries, foreign)
	}

	if err := r.unregisterHandoff("reuse-handoff", foreign, ""); err != nil {
		t.Fatalf("retire foreign generation: %v", err)
	}
	r.mu.Lock()
	entries = append([]reaperEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 0 {
		t.Fatalf("reaper retained %+v after retiring every generation", entries)
	}
}
