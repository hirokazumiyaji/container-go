package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

type imageIdentityRunner struct {
	mu       sync.Mutex
	calls    [][]string
	imageID  string
	platform string
}

func (r *imageIdentityRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	if len(args) == 3 && args[0] == "image" && args[1] == "inspect" {
		return []byte(fmt.Sprintf(`[{"Id":%q,"Os":"linux","Architecture":%q,"Variant":""}]`, r.imageID, r.platform)), nil, nil
	}
	return nil, nil, nil
}

func TestGenerationReplacedErrorTextRemainsCompatible(t *testing.T) {
	const want = "container was recreated; refusing to delete replaced container"
	if got := ErrGenerationReplaced.Error(); got != want {
		t.Fatalf("ErrGenerationReplaced = %q, want %q", got, want)
	}
}

type ociPlatformReuseRunner struct {
	*fakeRunner
	uid            string
	imageID        string
	imagePlatforms []string
	inspectCalls   int
	imageCalls     int
}

func (r *ociPlatformReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.inspectCalls++
		r.mu.Unlock()
		data := marshalReuseInspectJSON([]map[string]any{{
			"Id":       r.uid,
			"Name":     "/shared",
			"Platform": "linux",
			"Image":    r.imageID,
			"State":    map[string]string{"Status": "running"},
			"Config": map[string]any{
				"Image":  "redis:7-alpine",
				"Labels": reuseInspectLabels("aaaaaaaaaaaaaaaa"),
			},
			"NetworkSettings": map[string]any{},
		}})
		return data, nil, nil
	}
	if len(args) == 3 && args[0] == "image" && args[1] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		platform := "amd64"
		if r.imageCalls < len(r.imagePlatforms) {
			platform = r.imagePlatforms[r.imageCalls]
		}
		r.imageCalls++
		id := r.imageID
		r.mu.Unlock()
		return []byte(fmt.Sprintf(`[{"Id":%q,"Os":"linux","Architecture":%q}]`, id, platform)), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestWithReuseValidatesExistingAndFreshOCIPlatform(t *testing.T) {
	uid := strings.Repeat("e", 64)
	imageID := "sha256:" + strings.Repeat("f", 64)
	cases := []struct {
		name      string
		platforms []string
		wantErr   string
	}{
		{name: "existing mismatch", platforms: []string{"arm64"}, wantErr: "does not match existing"},
		{name: "fresh mismatch", platforms: []string{"amd64", "arm64"}, wantErr: "platform"},
		{name: "match", platforms: []string{"amd64", "amd64"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &ociPlatformReuseRunner{
				fakeRunner:     newTestRunner(),
				uid:            uid,
				imageID:        imageID,
				imagePlatforms: tc.platforms,
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(), WithPlatform("linux/amd64"),
				withRunner(runner), withEngine(dockerEngine{}))
			if tc.wantErr != "" {
				if ctr != nil || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Run = (%v, %v), want error containing %q", ctr, err, tc.wantErr)
				}
				return
			}
			if err != nil || ctr == nil {
				t.Fatalf("Run = (%v, %v), want success", ctr, err)
			}
		})
	}
}

func TestDockerPlatformResolutionUsesImmutableImageID(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("a", 64)
	runner := &imageIdentityRunner{imageID: imageID, platform: "amd64"}
	got, err := (dockerEngine{}).resolvePlatform(context.Background(), runner, &engineInfo{
		platform: "linux",
		image:    "redis:latest",
		imageID:  imageID,
	}, "linux/amd64")
	if err != nil {
		t.Fatalf("resolvePlatform: %v", err)
	}
	if got != "linux/amd64" {
		t.Fatalf("platform = %q, want linux/amd64", got)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "image inspect "+imageID {
		t.Fatalf("image inspect calls = %v, want immutable image ID", runner.calls)
	}
}

func TestDockerPlatformResolutionRejectsMutableOrMissingImageIdentity(t *testing.T) {
	for _, info := range []*engineInfo{
		{platform: "linux", image: "redis:latest"},
		{platform: "linux", image: "redis:latest", imageID: "redis:latest"},
	} {
		runner := &imageIdentityRunner{}
		if _, err := (dockerEngine{}).resolvePlatform(context.Background(), runner, info, "linux/amd64"); err == nil {
			t.Fatalf("resolvePlatform(%+v) = nil error, want fail-closed error", info)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("mutable image identity caused inspect: %v", runner.calls)
		}
	}
}

func TestResolveInfoPlatformDoesNotMutateSharedSnapshot(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("b", 64)
	runner := &imageIdentityRunner{imageID: imageID, platform: "arm64"}
	shared := &engineInfo{platform: "linux", image: "redis:latest", imageID: imageID}
	resolved, err := resolveInfoPlatform(context.Background(), dockerEngine{}, runner, shared, "linux/amd64")
	if err != nil {
		t.Fatalf("resolveInfoPlatform: %v", err)
	}
	if shared.platform != "linux" {
		t.Fatalf("shared platform mutated to %q", shared.platform)
	}
	if resolved.platform != "linux/arm64" || platformMatches("linux/amd64", resolved.platform) {
		t.Fatalf("resolved platform = %q, want actual linux/arm64", resolved.platform)
	}
}

func TestIdentityValidatorsFailClosed(t *testing.T) {
	empty := &engineInfo{}
	if sameReuseGeneration(empty, empty) {
		t.Fatal("two empty identities must not match")
	}
	invalidDocker := &engineInfo{uid: "short"}
	if sameReuseGeneration(invalidDocker, invalidDocker) {
		t.Fatal("two invalid Docker UIDs must not match")
	}
	if err := validateReuseIdentity(dockerEngine{}, &engineInfo{}); err == nil {
		t.Fatal("Docker reuse accepted missing UID")
	}
	if err := validateReuseIdentity(appleEngine{}, &engineInfo{labels: map[string]string{creationLabel: "bad"}}); err == nil {
		t.Fatal("Apple reuse accepted invalid generation")
	}
}

type identityOperationRunner struct {
	mu      sync.Mutex
	inspect []byte
	calls   [][]string
}

func (r *identityOperationRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "inspect" {
		return append([]byte(nil), r.inspect...), nil, nil
	}
	return nil, nil, nil
}

func (r *identityOperationRunner) callCount(subcommand string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, args := range r.calls {
		if len(args) > 0 && args[0] == subcommand {
			n++
		}
	}
	return n
}

func TestReusedAppleOperationRechecksGenerationBeforeNameOperation(t *testing.T) {
	runner := &identityOperationRunner{inspect: []byte(reuseInspectJSONWithCreation("myctr", "running", "redis:7-alpine", "aaaaaaaaaaaaaaaa"))}
	ctr := &Container{
		id:       "myctr",
		runner:   runner,
		eng:      appleEngine{},
		reused:   true,
		creation: "aaaaaaaaaaaaaaaa",
	}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if runner.callCount("stop") != 1 {
		t.Fatalf("stop calls = %d, want 1", runner.callCount("stop"))
	}
	runner.mu.Lock()
	runner.inspect = []byte(reuseInspectJSONWithCreation("myctr", "running", "redis:7-alpine", "bbbbbbbbbbbbbbbb"))
	runner.mu.Unlock()
	if err := ctr.Stop(context.Background(), nil); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("replacement Stop = %v, want ErrGenerationReplaced", err)
	}
	if runner.callCount("stop") != 1 {
		t.Fatalf("replacement issued another stop: %d", runner.callCount("stop"))
	}
}

type malformedReuseRunner struct {
	*fakeRunner
	payload []byte
}

func (r *malformedReuseRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "inspect" {
		r.mu.Lock()
		r.calls = append(r.calls, args)
		r.mu.Unlock()
		return append([]byte(nil), r.payload...), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestWithReuseRejectsMissingBackendIdentity(t *testing.T) {
	cases := []struct {
		name string
		eng  engine
		data []byte
	}{
		{
			name: "docker",
			eng:  dockerEngine{},
			data: dockerReuseInspectJSON(reuseInspectSpec{
				state:    "running",
				image:    "redis:7-alpine",
				creation: "aaaaaaaaaaaaaaaa",
				platform: "linux/amd64",
			}),
		},
		{
			name: "apple",
			eng:  appleEngine{},
			data: appleReuseInspectJSON(reuseInspectSpec{
				state:    "running",
				image:    "redis:7-alpine",
				creation: "invalid",
				platform: "linux/amd64",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &malformedReuseRunner{fakeRunner: newTestRunner(), payload: tc.data}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(), withRunner(runner), withEngine(tc.eng))
			if ctr != nil || err == nil || !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("Run = (%v, %v), want nil and ErrGenerationReplaced", ctr, err)
			}
			if runner.callWith("run") != nil || runner.callWith("delete") != nil {
				t.Fatalf("malformed identity caused create/delete: %v", runner.calls)
			}
		})
	}
}

func TestDockerOperationsUseOnlyValidatedImmutableUID(t *testing.T) {
	uid := strings.Repeat("c", 64)
	runner := &identityOperationRunner{}
	ctr := &Container{id: "myctr", runner: runner, eng: dockerEngine{}, uid: uid}
	if err := ctr.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	runner.mu.Lock()
	args := append([]string(nil), runner.calls[0]...)
	runner.mu.Unlock()
	if args[len(args)-1] != uid {
		t.Fatalf("stop target = %v, want UID %s", args, uid)
	}

	bad := &Container{id: "myctr", runner: runner, eng: dockerEngine{}, uid: "not-a-uid"}
	if err := bad.Stop(context.Background(), nil); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("invalid UID Stop = %v, want ErrGenerationReplaced", err)
	}
	if runner.callCount("stop") != 1 {
		t.Fatalf("invalid UID issued an operation: %d stops", runner.callCount("stop"))
	}
}

type waitErrorStrategy struct{ err error }

func (s waitErrorStrategy) WaitUntilReady(context.Context, wait.Target) error {
	if s.err != nil {
		return s.err
	}
	return errors.New("readiness probe failed")
}

func TestCleanupFailsClosedWhenDockerUIDIsMissing(t *testing.T) {
	runner := &failRunRunner{
		fakeRunner: newTestRunner(),
		creation:   "aaaaaaaaaaaaaaaa",
		inspectJSON: fmt.Sprintf(`[{"Id":"","Name":"/myctr","State":{"Status":"created"},"Config":{"Image":"redis:7-alpine","Labels":{%q:%q,%q:%q}},"NetworkSettings":{}}]`,
			managedLabel, "true", sessionLabel, sessionID()),
	}
	cfg := &config{runner: runner, eng: dockerEngine{}, name: "myctr", creation: runner.creation}
	cleanupFailedCreate(context.Background(), cfg, errors.New("run failed"), errors.New("run failed"))
	if len(runner.deleted) != 0 {
		t.Fatalf("cleanup deleted without Docker UID: %v", runner.deleted)
	}
}

func TestReturnedAppleHandleRejectsPostReturnReplacement(t *testing.T) {
	before := reuseInspectSpec{
		state:    "running",
		image:    "redis:7-alpine",
		creation: "aaaaaaaaaaaaaaaa",
		platform: "linux/amd64",
	}
	after := before
	after.creation = "bbbbbbbbbbbbbbbb"
	runner := newReuseTransitionRunner(appleReuseInspectJSON(before), appleReuseInspectJSON(after))
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	runner.replace()
	if err := ctr.Stop(context.Background(), nil); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("Stop after replacement = %v, want ErrGenerationReplaced", err)
	}
	if runner.callWith("stop") != nil {
		t.Fatal("replacement was addressed by name")
	}
}

func TestReuseWaitFailureJoinsGenerationReplacement(t *testing.T) {
	for _, backend := range reuseBackendFixtures() {
		t.Run(backend.name, func(t *testing.T) {
			before := backend.runningSpec()
			after := before
			if backend.name == "docker" {
				after.uid = strings.Repeat("d", 64)
			} else {
				after.creation = "bbbbbbbbbbbbbbbb"
			}
			runner := newReuseTransitionRunner(backend.inspect(before), backend.inspect(after))
			runner.replaceAfterFirst = true
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("shared"), WithReuse(), WithPlatform("linux/amd64"),
				withRunner(runner), withEngine(backend.eng),
				WithWaitStrategy(waitErrorStrategy{}),
			)
			if err == nil || !errors.Is(err, ErrGenerationReplaced) {
				t.Fatalf("Run error = %v, want joined ErrGenerationReplaced", err)
			}
			if !strings.HasPrefix(err.Error(), "reuse shared failed to become ready:") {
				t.Fatalf("error lost readiness prefix: %v", err)
			}
		})
	}
}

func TestReuseWaitNotFoundJoinsGenerationReplacement(t *testing.T) {
	before := reuseBackendFixtures()[0].runningSpec()
	runner := newReuseTransitionRunner(
		dockerReuseInspectJSON(before),
		dockerReuseInspectJSON(before),
	)
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("shared"), WithReuse(), withRunner(runner), withEngine(dockerEngine{}),
		WithWaitStrategy(waitErrorStrategy{err: fmt.Errorf("probe: %w", ErrContainerNotFound)}),
	)
	if err == nil || !errors.Is(err, ErrGenerationReplaced) || !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Run error = %v, want joined not-found and replacement errors", err)
	}
}
