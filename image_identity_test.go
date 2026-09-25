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
	issue115ImageIdentityOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue115ImageIdentityNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type issue115TagSwapRunner struct {
	mu            sync.Mutex
	pullDigest    string
	inspectDigest string
	swapOnInspect bool
	runImage      string
	pullCalls     int
	inspectCalls  int
}

func (r *issue115TagSwapRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "image" && len(args) > 1 && args[1] == "inspect":
		r.inspectCalls++
		digest := r.inspectDigest
		if digest == "" {
			digest = issue115ImageIdentityOld
		}
		if r.swapOnInspect {
			r.inspectDigest = issue115ImageIdentityNew
		}
		return []byte(fmt.Sprintf(`[{"Id":%q,"RepoDigests":["docker.io/library/redis@%s"]}]`, digest, digest)), nil, nil
	case args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull"):
		r.pullCalls++
		digest := r.pullDigest
		if digest == "" {
			digest = issue115ImageIdentityOld
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

func (r *issue115TagSwapRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func TestIssue115RunPinsDockerIdentityAfterPullMissingInspect(t *testing.T) {
	r := &issue115TagSwapRunner{inspectDigest: issue115ImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}), WithPullPolicy(PullMissing))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, issue115ImageIdentityOld) {
		t.Fatalf("run image = %q, want inspected identity %q", got, issue115ImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != issue115ImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, issue115ImageIdentityOld)
	}
}

func TestIssue115RunPinsDockerIdentityAfterPullAlways(t *testing.T) {
	r := &issue115TagSwapRunner{pullDigest: issue115ImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(dockerEngine{}), WithPullPolicy(PullAlways))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, issue115ImageIdentityOld) {
		t.Fatalf("run image = %q, want pulled identity %q", got, issue115ImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != issue115ImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, issue115ImageIdentityOld)
	}
}

type issue115AppleTagSwapRunner struct {
	mu            sync.Mutex
	pullDigest    string
	inspectDigest string
	swapOnInspect bool
	runImage      string
}

func (r *issue115AppleTagSwapRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "image" && len(args) > 1 && args[1] == "inspect":
		digest := r.inspectDigest
		if digest == "" {
			digest = issue115ImageIdentityOld
		}
		if r.swapOnInspect {
			r.inspectDigest = issue115ImageIdentityNew
		}
		return []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"name":"redis:7-alpine","descriptor":{"digest":%q}},"variants":[]}]`, digest, digest)), nil, nil
	case args[0] == "image" && len(args) > 1 && args[1] == "pull":
		digest := r.pullDigest
		if digest == "" {
			digest = issue115ImageIdentityOld
		}
		r.inspectDigest = digest
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	default:
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "unexpected command"}
	}
}

func (r *issue115AppleTagSwapRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func TestIssue115RunPinsAppleIdentityAfterTagReplacement(t *testing.T) {
	r := &issue115AppleTagSwapRunner{inspectDigest: issue115ImageIdentityOld, swapOnInspect: true}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}), WithPullPolicy(PullMissing))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, issue115ImageIdentityOld) {
		t.Fatalf("run image = %q, want inspected Apple identity %q", got, issue115ImageIdentityOld)
	}
	if !ctr.image.pinned || ctr.image.digest != issue115ImageIdentityOld {
		t.Fatalf("handle identity = %+v, want pinned digest %q", ctr.image, issue115ImageIdentityOld)
	}
}

func TestIssue115RunPinsAppleIdentityAfterPullAlways(t *testing.T) {
	r := &issue115AppleTagSwapRunner{pullDigest: issue115ImageIdentityOld, swapOnInspect: true}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}), WithPullPolicy(PullAlways))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.image(); !strings.Contains(got, issue115ImageIdentityOld) {
		t.Fatalf("run image = %q, want pulled Apple identity %q", got, issue115ImageIdentityOld)
	}
}

type issue115IdentityLessRunner struct {
	runImage string
	runCalls int
}

func (r *issue115IdentityLessRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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

func TestIssue115RunFailsClosedWhenIdentityIsUnavailable(t *testing.T) {
	r := &issue115IdentityLessRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued despite unavailable identity: %q", r.runImage)
	}
}

type issue115AppleNoDigestAliasRunner struct {
	runCalls int
}

func (r *issue115AppleNoDigestAliasRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "image" && len(args) > 1 && args[1] == "inspect" {
		if args[len(args)-1] == "redis:7-alpine" {
			return []byte(`[{"id":"` + issue115ImageIdentityOld + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + issue115ImageIdentityNew + `"}}}]`), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found"}
	}
	if args[0] == "run" {
		r.runCalls++
	}
	return nil, nil, nil
}

func TestIssue115PullNeverDoesNotSilentlyFetchApplePinnedDigest(t *testing.T) {
	r := &issue115AppleNoDigestAliasRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if r.runCalls != 0 {
		t.Fatal("PullNever issued run after pinned digest was not local")
	}

	fallback := &issue115AppleNoDigestAliasRunner{}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), WithAllowMutableImageTag(),
		withRunner(fallback), withEngine(appleEngine{}))
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

func TestIssue115PullNeverRejectsIdentitylessAppleInspect(t *testing.T) {
	r := &issue115IdentityLessRunner{}
	image := "redis:7-alpine@" + issue115ImageIdentityOld
	_, err := Run(context.Background(), image,
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued after identityless Apple inspect: %q", r.runImage)
	}
}

func TestIssue115AllowMutableImageTagIsExplicitFallback(t *testing.T) {
	r := &issue115IdentityLessRunner{}
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

func TestIssue115DockerFallsBackToImmutableImageIDWithoutRepoDigest(t *testing.T) {
	// Replace the normal RepoDigests response with an ID-only response.
	r2 := &issue115IDOnlyRunner{id: issue115ImageIdentityOld}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r2), withEngine(dockerEngine{}), WithPullPolicy(PullNever))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r2.runImage != issue115ImageIdentityOld {
		t.Fatalf("run image = %q, want Docker image ID %q", r2.runImage, issue115ImageIdentityOld)
	}
}

type issue115IDOnlyRunner struct {
	id       string
	runImage string
}

func (r *issue115IDOnlyRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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

func TestIssue115DockerImageIdentityUsesRepoDigestWithoutTag(t *testing.T) {
	data := []byte(`[{"Id":"` + issue115ImageIdentityOld + `","RepoDigests":["docker.io/library/redis@` + issue115ImageIdentityNew + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("image was reported absent")
	}
	want := "redis:7-alpine@" + issue115ImageIdentityNew
	if identity.reference != want || identity.digest != issue115ImageIdentityNew || !identity.pinned {
		t.Fatalf("identity = %+v, want reference %q", identity, want)
	}
}

func TestIssue115DockerImageIdentityFallsBackToIDForForeignRepoDigest(t *testing.T) {
	data := []byte(`[{"Id":"` + issue115ImageIdentityOld + `","RepoDigests":["evil.example/library/redis@` + issue115ImageIdentityNew + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.mismatch || !identity.pinned || identity.id != issue115ImageIdentityOld {
		t.Fatalf("identity = %+v, exists = %v, want immutable local ID fallback", identity, exists)
	}
}

func TestIssue115AppleImageIdentityUsesDescriptorDigest(t *testing.T) {
	data := []byte(`[{"id":"` + issue115ImageIdentityOld + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + issue115ImageIdentityNew + `"}},"variants":[]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("image was reported absent")
	}
	want := "redis:7-alpine@" + issue115ImageIdentityNew
	if identity.reference != want || identity.digest != issue115ImageIdentityNew || !identity.pinned {
		t.Fatalf("identity = %+v, want reference %q", identity, want)
	}
}

func TestIssue115AppleImageIdentityUsesIDAsDigestReference(t *testing.T) {
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

func TestIssue115AppleImageIdentityUsesSelectedPlatformVariant(t *testing.T) {
	data := []byte(`[{"id":"` + issue115ImageIdentityOld + `","configuration":{"name":"redis:7-alpine","descriptor":{"digest":"` + issue115ImageIdentityOld + `"}},"variants":[{"digest":"` + issue115ImageIdentityNew + `","platform":{"os":"linux","architecture":"arm64"}}]}]`)
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "linux/arm64")
	if !exists {
		t.Fatal("selected platform was reported absent")
	}
	if identity.digest != issue115ImageIdentityNew {
		t.Fatalf("identity = %+v, want selected variant %q", identity, issue115ImageIdentityNew)
	}
}

func TestIssue115ImageIdentityMismatchCannotUseMutableFallback(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(
		[]byte(`[{"id":"`+issue115ImageIdentityOld+`","configuration":{"name":"nginx:alpine","descriptor":{"digest":"`+issue115ImageIdentityNew+`"}}}]`),
		"redis:7-alpine", "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want mismatch", identity, exists)
	}
	if _, err := (&config{eng: appleEngine{}, allowMutableImageTag: true}).pinImage("redis:7-alpine", identity); !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("pinImage error = %v, want ErrImageIdentityMismatch", err)
	}
}
