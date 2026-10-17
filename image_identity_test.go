package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	testImageIdentityOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testImageIdentityNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type tagSwapRunner struct {
	mu            sync.Mutex
	pullDigest    string
	inspectDigest string
	swapOnInspect bool
	runImage      string
	pullCalls     int
	inspectCalls  int
}

func (r *tagSwapRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "image" && len(args) > 1 && args[1] == "inspect":
		r.inspectCalls++
		digest := r.inspectDigest
		if digest == "" {
			digest = testImageIdentityOld
		}
		if r.swapOnInspect {
			r.inspectDigest = testImageIdentityNew
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"RepoDigests":["docker.io/library/redis@%s"]}]`, digest, digest)), nil, nil
	case args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull"):
		r.pullCalls++
		digest := r.pullDigest
		if digest == "" {
			digest = testImageIdentityOld
		}
		r.inspectDigest = digest
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		return []byte(strings.Repeat("c", 64) + "\n"), nil, nil
	default:
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "unexpected command"}
	}
}

func (r *tagSwapRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func TestRunPinsDockerIdentityAfterPullMissingInspect(t *testing.T) {
	r := &tagSwapRunner{inspectDigest: testImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}), WithPullPolicy(PullMissing))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, testImageIdentityOld) {
		t.Fatalf("run image = %q, want inspected identity %q", got, testImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != testImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, testImageIdentityOld)
	}
}

func TestRunPinsDockerIdentityAfterPullAlways(t *testing.T) {
	r := &tagSwapRunner{pullDigest: testImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}), WithPullPolicy(PullAlways))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, testImageIdentityOld) {
		t.Fatalf("run image = %q, want pulled identity %q", got, testImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != testImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, testImageIdentityOld)
	}
}

type appleTagSwapRunner struct {
	mu             sync.Mutex
	pullDigest     string
	inspectDigest  string
	resolvedDigest string
	swapOnInspect  bool
	runImage       string
	creation       string
}

func (r *appleTagSwapRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "image" && len(args) > 1 && args[1] == "inspect":
		digest := r.inspectDigest
		if digest == "" {
			digest = testImageIdentityOld
		}
		if args[len(args)-1] == "redis:7-alpine" {
			// The mutable tag may be replaced after the initial resolution,
			// but the addressability check for the already-pinned reference
			// must continue to observe that exact old digest.
			r.resolvedDigest = digest
			if r.swapOnInspect {
				r.inspectDigest = testImageIdentityNew
			}
		} else if r.resolvedDigest != "" {
			digest = r.resolvedDigest
		}
		return freshAppleImageJSON(digest, digest), nil, nil
	case args[0] == "image" && len(args) > 1 && args[1] == "pull":
		digest := r.pullDigest
		if digest == "" {
			digest = testImageIdentityOld
		}
		r.inspectDigest = digest
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				r.creation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		image := stripImageDigest(r.runImage)
		if image == "" {
			image = "redis:7-alpine"
		}
		digest := r.resolvedDigest
		if digest == "" {
			digest = testImageIdentityOld
		}
		return platformPinningContainerJSON(image, digest, freshHostPlatform(), r.creation), nil, nil
	default:
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "unexpected command"}
	}
}

func (r *appleTagSwapRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func TestRunPinsAppleIdentityAfterTagReplacement(t *testing.T) {
	r := &appleTagSwapRunner{inspectDigest: testImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}), WithPullPolicy(PullMissing))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, testImageIdentityOld) {
		t.Fatalf("run image = %q, want inspected Apple identity %q", got, testImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != testImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, testImageIdentityOld)
	}
}

func TestRunPinsAppleIdentityAfterPullAlways(t *testing.T) {
	r := &appleTagSwapRunner{pullDigest: testImageIdentityOld, swapOnInspect: true}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}), WithPullPolicy(PullAlways))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, testImageIdentityOld) {
		t.Fatalf("run image = %q, want pulled Apple identity %q", got, testImageIdentityOld)
	}
}

type identityLessRunner struct {
	runImage string
	runCalls int
}

func (r *identityLessRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch {
	case args[0] == "image" && len(args) > 1 && args[1] == "inspect":
		return []byte(`[{"reference":"redis:7-alpine"}]`), nil, nil
	case args[0] == "run":
		r.runCalls++
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestRunFailsClosedWhenIdentityIsUnavailable(t *testing.T) {
	r := &identityLessRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued despite unavailable identity: %q", r.runImage)
	}
}

type appleNoDigestAliasRunner struct {
	runCalls int
}

func (r *appleNoDigestAliasRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "image" && len(args) > 1 && args[1] == "inspect" {
		if args[len(args)-1] == "redis:7-alpine" {
			return freshAppleImageJSON(testImageIdentityNew, testImageIdentityNew), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found: " + args[len(args)-1]}
	}
	if args[0] == "run" {
		r.runCalls++
	}
	return nil, nil, nil
}

func TestPullNeverDoesNotSilentlyFetchApplePinnedDigest(t *testing.T) {
	r := &appleNoDigestAliasRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if r.runCalls != 0 {
		t.Fatal("PullNever issued run after pinned digest was not local")
	}

	fallback := &appleNoDigestAliasRunner{}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithAllowMutableImageTag(),
		withRunner(fallback), withEngine(applePullNeverBelowPreflight{}))
	if err != nil {
		t.Fatalf("explicit fallback Run: %v", err)
	}
	if ctr.image.pinned {
		t.Fatal("explicit fallback unexpectedly retained pinned identity")
	}
	if fallback.runCalls != 1 {
		t.Fatalf("fallback run calls = %d, want 1", fallback.runCalls)
	}
}

func TestPullNeverRejectsIdentitylessAppleInspect(t *testing.T) {
	r := &identityLessRunner{}
	image := "redis:7-alpine@" + testImageIdentityOld
	_, err := Run(context.Background(), image,
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued after identityless Apple inspect: %q", r.runImage)
	}
}

func TestAllowMutableImageTagIsExplicitFallback(t *testing.T) {
	r := &identityLessRunner{}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithAllowMutableImageTag(), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.runImage != "redis:7-alpine" {
		t.Fatalf("run image = %q, want explicit mutable-tag fallback", r.runImage)
	}
	if ctr.image.pinned {
		t.Fatal("fallback handle marked image as pinned")
	}
}

func TestDockerFallsBackToImmutableImageIDWithoutRepoDigest(t *testing.T) {
	// Replace the normal RepoDigests response with an ID-only response.
	r2 := &idOnlyRunner{id: testImageIdentityOld}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r2), withEngine(dockerEngine{}), WithPullPolicy(PullNever))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r2.runImage != testImageIdentityOld {
		t.Fatalf("run image = %q, want Docker image ID %q", r2.runImage, testImageIdentityOld)
	}
}

type idOnlyRunner struct {
	id       string
	runImage string
}

func (r *idOnlyRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "image":
		return []byte(fmt.Sprintf(`[{"Id":%q}]`, r.id)), nil, nil
	case "run":
		r.runImage = args[len(args)-1]
		return []byte(strings.Repeat("d", 64) + "\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func TestDockerImageIdentityUsesRepoDigestWithoutTag(t *testing.T) {
	data := []byte(`[{"Id":"` + testImageIdentityOld + `","RepoDigests":["docker.io/library/redis@` + testImageIdentityNew + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("image was reported absent")
	}
	want := "redis:7-alpine@" + testImageIdentityNew
	if identity.reference != want || identity.digest != testImageIdentityNew || !identity.pinned {
		t.Fatalf("identity = %+v, want reference %q", identity, want)
	}
}

func TestDockerImageIdentityFallsBackToIDForForeignRepoDigest(t *testing.T) {
	data := []byte(`[{"Id":"` + testImageIdentityOld + `","RepoDigests":["evil.example/library/redis@` + testImageIdentityNew + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.mismatch || !identity.pinned || identity.id != testImageIdentityOld {
		t.Fatalf("identity = %+v, exists = %v, want immutable local ID fallback", identity, exists)
	}
}

func TestAppleImageIdentityUsesDescriptorDigest(t *testing.T) {
	data := []byte(`[{"id":"` + testImageIdentityOld + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + testImageIdentityNew + `"}},"variants":[]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("image was reported absent")
	}
	want := "redis:7-alpine@" + testImageIdentityNew
	if identity.reference != want || identity.digest != testImageIdentityNew || !identity.pinned {
		t.Fatalf("identity = %+v, want reference %q", identity, want)
	}
}

func TestAppleImageIdentityUsesIDAsDigestReference(t *testing.T) {
	data := []byte(`[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","configuration":{"name":"redis:7-alpine"},"variants":[]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("image was reported absent")
	}
	want := "redis:7-alpine@sha256:" + strings.Repeat("a", 64)
	if identity.reference != want || identity.digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("identity = %+v, want digest reference %q", identity, want)
	}
}

func TestAppleImageIdentityUsesRootForSelectedPlatformVariant(t *testing.T) {
	data := []byte(`[{"id":"` + testImageIdentityOld + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + testImageIdentityOld + `"}},"variants":[{"digest":"` + testImageIdentityNew + `","platform":{"os":"linux","architecture":"arm64"}}]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "linux/arm64")
	if !exists {
		t.Fatal("selected platform was reported absent")
	}
	if identity.digest != testImageIdentityOld || identity.reference != "redis:7-alpine@"+testImageIdentityOld {
		t.Fatalf("identity = %+v, want root descriptor identity", identity)
	}
	if identity.variantDigest != testImageIdentityNew || identity.platform != "linux/arm64" {
		t.Fatalf("identity = %+v, want separately tracked platform variant", identity)
	}
}

func TestImageIdentityMismatchCannotUseMutableFallback(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(
		[]byte(`[{"id":"`+testImageIdentityOld+`","configuration":{"name":"nginx:alpine","descriptor":{"digest":"`+testImageIdentityNew+`"}}}]`),
		"redis:7-alpine", "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want mismatch", identity, exists)
	}
	if _, err := (&config{eng: appleEngine{}, allowMutableImageTag: true}).pinImage("redis:7-alpine", identity); !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("pinImage error = %v, want ErrImageIdentityMismatch", err)
	}
}
