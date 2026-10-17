package container

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	reuseImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reuseOtherDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reuseImageID     = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type reuseRunner struct {
	mu sync.Mutex

	created         bool
	imageDigest     string
	containerDigest string
	containerImage  string
	imageID         string
	docker          bool
	deleted         bool
	runImage        string
	creation        string
}

func (r *reuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		if r.imageID != "" {
			return fixtureJSONBytes([]map[string]any{{"Id": r.imageID}}), nil, nil
		}
		return fixtureJSONBytes([]map[string]any{{
			"id": r.imageDigest[7:],
			"configuration": map[string]any{
				"name":       r.containerImage,
				"descriptor": map[string]string{"digest": r.imageDigest},
			},
			"variants": []any{freshVariant(freshHostPlatform(), r.imageDigest)},
		}}), nil, nil
	case args[0] == "run":
		r.created = true
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			if arg == "--label" && i+1 < len(args) {
				if value, ok := strings.CutPrefix(args[i+1], creationLabel+"="); ok {
					r.creation = value
				}
			}
		}
		if r.docker {
			return []byte("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n"), nil, nil
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		if !r.created {
			bin := "container"
			if r.docker {
				bin = "docker"
			}
			return nil, nil, &cli.CLIError{Binary: bin, Args: args, ExitCode: 1, Stderr: "Error: container not found: myctr\nError: No such object: myctr"}
		}
		creation := r.creation
		if creation == "" {
			creation = "0123456789abcdef"
		}
		if r.docker {
			return fixtureJSONBytes([]map[string]any{{
				"Id":         "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				"Image":      r.imageID,
				"HostConfig": map[string]any{"NetworkMode": "bridge"},
				"Config": map[string]any{
					"Image": r.containerImage,
					"Labels": map[string]string{
						managedLabel:  "true",
						reuseLabel:    "true",
						creationLabel: creation,
					},
				},
				"State": map[string]string{"Status": "running"},
				"NetworkSettings": map[string]any{
					"Networks": map[string]any{"bridge": map[string]any{}},
					"Ports":    map[string]any{},
				},
			}}), nil, nil
		}
		return fixtureJSONBytes([]map[string]any{{
			"id": "myctr",
			"configuration": map[string]any{
				"id": "myctr",
				"image": map[string]any{
					"reference":     r.containerImage,
					"descriptor":    map[string]string{"digest": r.containerDigest},
					"variantDigest": r.containerDigest,
				},
				"platform": freshPlatformMap(),
				"labels": map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: creation,
				},
			},
			"status": map[string]any{
				"state":    "running",
				"networks": []map[string]string{{"ipv4Address": "192.168.64.3/24"}},
			},
		}}), nil, nil
	case args[0] == "delete" || args[0] == "rm":
		r.deleted = true
		return nil, nil, nil
	case args[0] == "version":
		return []byte("linux\n"), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *reuseRunner) state() (created, deleted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created, r.deleted
}

func fixtureJSONBytes(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func TestReuseRejectsResolvedDigestMismatch(t *testing.T) {
	r := &reuseRunner{
		created:         true,
		imageDigest:     reuseOtherDigest,
		containerDigest: reuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want resolved-image mismatch", err)
	}
}

func TestReuseResolvesEachCallersIdentity(t *testing.T) {
	r := &reuseRunner{
		created:         true,
		imageDigest:     reuseImageDigest,
		containerDigest: reuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{})); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// The mutable tag now resolves to another image. A later caller must
	// not reuse the first caller's identity from the shared flight.
	r.mu.Lock()
	r.imageDigest = reuseOtherDigest
	r.mu.Unlock()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("second Run error = %v, want resolved-image mismatch", err)
	}
}

func TestReuseUsesIDWhenContainerOnlyReportsAnAlias(t *testing.T) {
	r := &reuseRunner{
		created:         true,
		imageID:         reuseImageID,
		docker:          true,
		containerImage:  "other:tag",
		containerDigest: "",
	}
	// The Docker-style image ID is the only immutable identity available;
	// the container's mutable Config.Image is a different alias.
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(dockerEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestReuseCleansUpWhenPostCreateValidationFails(t *testing.T) {
	r := &reuseRunner{
		imageDigest:     reuseImageDigest,
		containerDigest: reuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), WithPublishedPort("127.0.0.1:16379:6379"),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "published port") {
		t.Fatalf("error = %v, want post-create port validation error", err)
	}
	created, deleted := r.state()
	if !created || !deleted {
		t.Fatalf("created = %v, deleted = %v, want newly-created container removed", created, deleted)
	}
}

func TestReuseCleansUpAfterIdentityValidationFailure(t *testing.T) {
	r := &reuseRunner{
		imageDigest:     reuseOtherDigest,
		containerDigest: reuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want post-create image mismatch", err)
	}
	created, deleted := r.state()
	if !created || !deleted {
		t.Fatalf("created = %v, deleted = %v, want newly-created container removed", created, deleted)
	}
}

func TestDockerForeignRepoDigestFallsBackToID(t *testing.T) {
	data := []byte(`[{"Id":"` + reuseImageID + `","RepoDigests":["evil.example/library/redis@` + reuseOtherDigest + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want ID fallback", identity, exists)
	}
	if identity.reference != reuseImageID || identity.id != reuseImageID || !identity.pinned {
		t.Fatalf("identity = %+v, want pinned Docker image ID %q", identity, reuseImageID)
	}
}

func TestDockerExplicitPinnedRepoConflictIsMismatch(t *testing.T) {
	data := []byte(`[{"Id":"` + reuseImageID + `","RepoDigests":["evil.example/library/redis@` + reuseOtherDigest + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine@"+reuseOtherDigest, "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want explicit pinned conflict", identity, exists)
	}

	// A differing digest is also a conflict when the repository is
	// foreign; the ID fallback is reserved for unpinned local aliases.
	identity, exists = (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine@"+reuseImageDigest, "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want digest conflict", identity, exists)
	}
}

func TestAppleLocalIDDigestUsesMutableFallbackWhenAllowed(t *testing.T) {
	r := &appleIDRunner{}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing), WithAllowMutableImageTag(),
		withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.pinned {
		t.Fatal("mutable fallback unexpectedly pinned")
	}
	if r.image() != "redis:7-alpine" {
		t.Fatalf("run image = %q, want original mutable tag", r.image())
	}
}

func TestAppleLocalIDDigestFailsClosedWithoutFallback(t *testing.T) {
	r := &appleIDRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing),
		withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("known local-address absence was reported as identity-unavailable: %v", err)
	}
	if r.image() != "" {
		t.Fatalf("run image = %q, want no run", r.image())
	}
}

type appleIDRunner struct {
	mu       sync.Mutex
	runImage string
}

func (r *appleIDRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func (r *appleIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		if args[len(args)-1] == "redis:7-alpine" {
			return []byte(`[{"id":"` + reuseImageID[7:] + `","configuration":{"name":"redis:7-alpine"},"variants":[]}]`), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found: " + args[len(args)-1]}
	}
	if args[0] == "run" {
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	}
	return nil, nil, nil
}

func TestPullNeverRejectsIdentitylessAppleInspectForPinnedInput(t *testing.T) {
	r := &identityLessRunner{}
	image := "redis:7-alpine@" + reuseImageDigest
	_, err := Run(context.Background(), image,
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued after identityless Apple inspect: %q", r.runImage)
	}
}

func TestAppleDigestReferenceIsNormalizedForRun(t *testing.T) {
	r := &appleTagSwapRunner{pullDigest: reuseImageDigest}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullAlways), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.reference != "redis:7-alpine@"+reuseImageDigest {
		t.Fatalf("reference = %q, want normalized Apple digest reference", ctr.image.reference)
	}
}
