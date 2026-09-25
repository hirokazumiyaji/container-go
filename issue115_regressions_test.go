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
	issue115ReuseImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue115ReuseOtherDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	issue115ReuseImageID     = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type issue115ReuseRunner struct {
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

func (r *issue115ReuseRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		if r.imageID != "" {
			return issue115JSONBytes([]map[string]any{{"Id": r.imageID}}), nil, nil
		}
		return issue115JSONBytes([]map[string]any{{
			"id": r.imageDigest[7:],
			"configuration": map[string]any{
				"name":       r.containerImage,
				"descriptor": map[string]string{"digest": r.imageDigest},
			},
			"variants": []any{},
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
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		if !r.created {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: `not found: "myctr"`}
		}
		if r.docker {
			return issue115JSONBytes([]map[string]any{{
				"Id":    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				"Image": r.imageID,
				"Config": map[string]any{
					"Image": r.containerImage,
					"Labels": map[string]string{
						managedLabel:  "true",
						reuseLabel:    "true",
						creationLabel: r.creation,
					},
				},
				"State":           map[string]string{"Status": "running"},
				"NetworkSettings": map[string]any{"Ports": map[string]any{}},
			}}), nil, nil
		}
		return issue115JSONBytes([]map[string]any{{
			"id": "myctr",
			"configuration": map[string]any{
				"id": "myctr",
				"image": map[string]any{
					"reference":  r.containerImage,
					"descriptor": map[string]string{"digest": r.containerDigest},
				},
				"labels": map[string]string{
					managedLabel:  "true",
					reuseLabel:    "true",
					creationLabel: r.creation,
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
	default:
		return nil, nil, nil
	}
}

func (r *issue115ReuseRunner) state() (created, deleted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created, r.deleted
}

func issue115JSONBytes(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func TestIssue115ReuseRejectsResolvedDigestMismatch(t *testing.T) {
	r := &issue115ReuseRunner{
		created:         true,
		imageDigest:     issue115ReuseOtherDigest,
		containerDigest: issue115ReuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want resolved-image mismatch", err)
	}
}

func TestIssue115ReuseResolvesEachCallersIdentity(t *testing.T) {
	r := &issue115ReuseRunner{
		created:         true,
		imageDigest:     issue115ReuseImageDigest,
		containerDigest: issue115ReuseImageDigest,
		containerImage:  "redis:7-alpine",
	}
	if _, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{})); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// The mutable tag now resolves to another image. A later caller must
	// not reuse the first caller's identity from the shared flight.
	r.mu.Lock()
	r.imageDigest = issue115ReuseOtherDigest
	r.mu.Unlock()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithReuse(), withRunner(r), withEngine(appleEngine{}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("second Run error = %v, want resolved-image mismatch", err)
	}
}

func TestIssue115ReuseUsesIDWhenContainerOnlyReportsAnAlias(t *testing.T) {
	r := &issue115ReuseRunner{
		created:         true,
		imageID:         issue115ReuseImageID,
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

func TestIssue115ReuseCleansUpWhenPostCreateValidationFails(t *testing.T) {
	r := &issue115ReuseRunner{
		imageDigest:     issue115ReuseImageDigest,
		containerDigest: issue115ReuseImageDigest,
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

func TestIssue115ReuseCleansUpAfterIdentityValidationFailure(t *testing.T) {
	r := &issue115ReuseRunner{
		imageDigest:     issue115ReuseOtherDigest,
		containerDigest: issue115ReuseImageDigest,
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

func TestIssue115DockerForeignRepoDigestFallsBackToID(t *testing.T) {
	data := []byte(`[{"Id":"` + issue115ReuseImageID + `","RepoDigests":["evil.example/library/redis@` + issue115ReuseOtherDigest + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists || identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want ID fallback", identity, exists)
	}
	if identity.reference != issue115ReuseImageID || identity.id != issue115ReuseImageID || !identity.pinned {
		t.Fatalf("identity = %+v, want pinned Docker image ID %q", identity, issue115ReuseImageID)
	}
}

func TestIssue115DockerExplicitPinnedRepoConflictIsMismatch(t *testing.T) {
	data := []byte(`[{"Id":"` + issue115ReuseImageID + `","RepoDigests":["evil.example/library/redis@` + issue115ReuseOtherDigest + `"]}]`)
	identity, exists := (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine@"+issue115ReuseOtherDigest, "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want explicit pinned conflict", identity, exists)
	}

	// A differing digest is also a conflict when the repository is
	// foreign; the ID fallback is reserved for unpinned local aliases.
	identity, exists = (dockerEngine{}).parseImageIdentity(data, "redis:7-alpine@"+issue115ReuseImageDigest, "")
	if !exists || !identity.mismatch {
		t.Fatalf("identity = %+v, exists = %v, want digest conflict", identity, exists)
	}
}

func TestIssue115AppleLocalIDDigestUsesMutableFallbackWhenAllowed(t *testing.T) {
	r := &issue115AppleIDRunner{}
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

func TestIssue115AppleLocalIDDigestFailsClosedWithoutFallback(t *testing.T) {
	r := &issue115AppleIDRunner{}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing),
		withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.image() != "" {
		t.Fatalf("run image = %q, want no run", r.image())
	}
}

type issue115AppleIDRunner struct {
	mu       sync.Mutex
	runImage string
}

func (r *issue115AppleIDRunner) image() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage
}

func (r *issue115AppleIDRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		if args[len(args)-1] == "redis:7-alpine" {
			return []byte(`[{"id":"` + issue115ReuseImageID[7:] + `","configuration":{"name":"redis:7-alpine"},"variants":[]}]`), nil, nil
		}
		return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found"}
	}
	if args[0] == "run" {
		r.runImage = args[len(args)-1]
		return []byte("myctr\n"), nil, nil
	}
	return nil, nil, nil
}

func TestIssue115PullNeverRejectsIdentitylessAppleInspectForPinnedInput(t *testing.T) {
	r := &issue115IdentityLessRunner{}
	image := "redis:7-alpine@" + issue115ReuseImageDigest
	_, err := Run(context.Background(), image,
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run issued after identityless Apple inspect: %q", r.runImage)
	}
}

func TestIssue115AppleDigestReferenceIsNormalizedForRun(t *testing.T) {
	r := &issue115AppleTagSwapRunner{pullDigest: issue115ReuseImageDigest}
	ctr, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullAlways), withRunner(r), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.reference != "redis:7-alpine@"+issue115ReuseImageDigest {
		t.Fatalf("reference = %q, want normalized Apple digest reference", ctr.image.reference)
	}
}
