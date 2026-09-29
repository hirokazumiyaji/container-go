package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const issue115AppleImageID = "3333333333333333333333333333333333333333333333333333333333333333"

func issue115AppleImageFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/apple_image_inspect_v1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type issue115AppleAddressRunner struct {
	mu sync.Mutex

	fixture           []byte
	localMissing      bool
	localIdentityless bool
	localInspectErr   error
	exactPullErr      error
	originalPullErr   error
	runImage          string
	platform          string
	inspectTargets    []string
	pullTargets       []string
}

func (r *issue115AppleAddressRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		target := args[len(args)-1]
		r.inspectTargets = append(r.inspectTargets, target)
		if target == "redis:7-alpine" {
			return r.fixture, nil, nil
		}
		if r.localInspectErr != nil {
			return nil, nil, r.localInspectErr
		}
		if r.localMissing {
			return nil, nil, &cli.CLIError{Args: args, ExitCode: 1, Stderr: "image not found: " + target}
		}
		if r.localIdentityless {
			return []byte(`[{"configuration":{"name":"redis:7-alpine"}}]`), nil, nil
		}
		return r.fixture, nil, nil
	case len(args) >= 2 && args[0] == "image" && args[1] == "pull":
		target := args[len(args)-1]
		r.pullTargets = append(r.pullTargets, target)
		if target == "redis:7-alpine" {
			return nil, nil, r.originalPullErr
		}
		if r.exactPullErr != nil {
			return nil, nil, r.exactPullErr
		}
		r.localMissing = false
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			if arg == "--platform" && i+1 < len(args) {
				r.platform = args[i+1]
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		image := stripImageDigest(r.runImage)
		if image == "" {
			image = "redis:7-alpine"
		}
		platform := r.platform
		if platform == "" {
			platform = "linux/arm64/v8"
		}
		return issue115ReviewContainerJSON(image, "sha256:1111111111111111111111111111111111111111111111111111111111111111", platform), nil, nil
	default:
		return nil, nil, nil
	}
}

func (r *issue115AppleAddressRunner) state() (runImage string, inspectTargets, pullTargets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runImage, append([]string(nil), r.inspectTargets...), append([]string(nil), r.pullTargets...)
}

func TestIssue115AppleFixtureUsesImageResourceDescriptor(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(issue115AppleImageFixture(t), "docker.io/library/redis:7-alpine", "")
	if !exists {
		t.Fatal("realistic Apple image fixture was reported absent")
	}
	if !identity.pinned || identity.id != "" {
		t.Fatalf("identity = %+v, want descriptor-backed identity without Docker ID", identity)
	}
	if identity.digest != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("digest = %q, want fixture descriptor", identity.digest)
	}

	variant, exists := (appleEngine{}).parseImageIdentity(issue115AppleImageFixture(t), "docker.io/library/redis:7-alpine", "linux/arm64/v8")
	if !exists || variant.digest != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("variant identity = %+v, exists = %v, want root descriptor", variant, exists)
	}
	if variant.variantDigest != "sha256:2222222222222222222222222222222222222222222222222222222222222222" || variant.platform != "linux/arm64/v8" {
		t.Fatalf("variant metadata = %+v, want selected platform digest", variant)
	}
}

func TestIssue115AppleDescriptorReferenceIsLocallyCheckedForAllPullPolicies(t *testing.T) {
	for _, policy := range []PullPolicy{PullMissing, PullAlways} {
		t.Run(policyName(policy), func(t *testing.T) {
			r := &issue115AppleAddressRunner{
				fixture:      issue115AppleImageFixture(t),
				localMissing: true,
				exactPullErr: &cli.CLIError{Args: []string{"image", "pull", "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"}, ExitCode: 1, Stderr: "image not found: pinned"},
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(policy), withRunner(r), withEngine(appleEngine{}))
			if !errors.Is(err, ErrImageIdentityNotLocal) {
				t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
			}
			if errors.Is(err, ErrImageIdentityUnavailable) {
				t.Fatalf("known absence must not be reported as identity-unavailable: %v", err)
			}
			runImage, inspectTargets, pullTargets := r.state()
			if runImage != "" {
				t.Fatalf("run was issued with an unaddressable pinned reference: %q", runImage)
			}
			if len(inspectTargets) < 2 || inspectTargets[1] != "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111" {
				t.Fatalf("pinned reference was not inspected: %v", inspectTargets)
			}
			if len(pullTargets) == 0 || pullTargets[len(pullTargets)-1] != "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111" {
				t.Fatalf("expected controlled exact-digest pull: %v", pullTargets)
			}
		})
	}
}

func TestIssue115AppleDescriptorReferenceUsesLocalExactReference(t *testing.T) {
	for _, policy := range []PullPolicy{PullMissing, PullAlways} {
		t.Run(policyName(policy), func(t *testing.T) {
			r := &issue115AppleAddressRunner{fixture: issue115AppleImageFixture(t)}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(policy), withRunner(r), withEngine(appleEngine{}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !ctr.image.pinned {
				t.Fatal("descriptor-backed reference was not pinned")
			}
			runImage, inspectTargets, pullTargets := r.state()
			want := "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
			if runImage != want {
				t.Fatalf("run image = %q, want %q", runImage, want)
			}
			if len(inspectTargets) < 2 || inspectTargets[1] != want {
				t.Fatalf("exact local reference was not inspected: %v", inspectTargets)
			}
			for _, target := range pullTargets {
				if strings.Contains(target, "@") {
					t.Fatalf("unexpected exact-digest pull for an already-local reference: %v", pullTargets)
				}
			}
		})
	}
}

func TestIssue115AppleDescriptorReferenceUsesExplicitMutableFallback(t *testing.T) {
	for _, policy := range []PullPolicy{PullMissing, PullAlways} {
		t.Run(policyName(policy), func(t *testing.T) {
			r := &issue115AppleAddressRunner{
				fixture:      issue115AppleImageFixture(t),
				localMissing: true,
				exactPullErr: &cli.CLIError{Args: []string{"image", "pull", "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"}, ExitCode: 1, Stderr: "image not found: pinned"},
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(policy), WithAllowMutableImageTag(),
				withRunner(r), withEngine(appleEngine{}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr.image.pinned {
				t.Fatal("explicit mutable fallback was marked pinned")
			}
			runImage, _, _ := r.state()
			if runImage != "redis:7-alpine" {
				t.Fatalf("run image = %q, want original mutable tag", runImage)
			}
		})
	}
}

func TestIssue115AppleAddressInspectErrorsRemainOperationalErrors(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		check func(*testing.T, error)
	}{
		{
			name: "permission",
			err:  &cli.CLIError{Args: []string{"image", "inspect", "pinned"}, ExitCode: 1, Stderr: "permission denied: image not found"},
			check: func(t *testing.T, err error) {
				var cliErr *cli.CLIError
				if !errors.As(err, &cliErr) {
					t.Fatalf("error = %v, want CLI permission error", err)
				}
			},
		},
		{
			name: "cancel",
			err:  context.Canceled,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &issue115AppleAddressRunner{
				fixture:         issue115AppleImageFixture(t),
				localInspectErr: tc.err,
			}
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithPullPolicy(PullNever), WithAllowMutableImageTag(),
				withRunner(r), withEngine(appleEngine{}))
			if err == nil {
				t.Fatal("want local-address inspection error")
			}
			tc.check(t, err)
			if errors.Is(err, ErrImageIdentityUnavailable) || errors.Is(err, ErrImageIdentityNotLocal) {
				t.Fatalf("transport/permission/cancel error was misclassified: %v", err)
			}
			if runImage, _, _ := r.state(); runImage != "" {
				t.Fatalf("run was issued after failed address verification: %q", runImage)
			}
		})
	}
}

func TestIssue115AppleAddressPullErrorDoesNotUseMutableFallback(t *testing.T) {
	r := &issue115AppleAddressRunner{
		fixture:      issue115AppleImageFixture(t),
		localMissing: true,
		exactPullErr: &cli.CLIError{Args: []string{"image", "pull", "pinned"}, ExitCode: 1, Stderr: "permission denied: image not found"},
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing), WithAllowMutableImageTag(),
		withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("want exact-digest pull permission error")
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want CLI permission error", err)
	}
	if errors.Is(err, ErrImageIdentityUnavailable) || errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("pull transport/permission error was misclassified: %v", err)
	}
	if runImage, _, _ := r.state(); runImage != "" {
		t.Fatalf("run was issued after pull failure: %q", runImage)
	}
}

func TestIssue115AppleIdentitylessInspectDoesNotTrustCallerDigest(t *testing.T) {
	r := &issue115IdentityLessRunner{}
	image := "redis:7-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	_, err := Run(context.Background(), image,
		WithName("myctr"), WithPullPolicy(PullMissing), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if r.runCalls != 0 {
		t.Fatalf("run was issued after identity-less descriptor check: %q", r.runImage)
	}
}

func TestIssue115MalformedAppleInspectFailsClosedWithoutPull(t *testing.T) {
	r := &issue115AppleAddressRunner{fixture: []byte("{")}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullMissing), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	runImage, _, pullTargets := r.state()
	if runImage != "" || len(pullTargets) != 0 {
		t.Fatalf("malformed inspect caused run=%q pulls=%v", runImage, pullTargets)
	}
}

func TestIssue115AppleIdentitylessAddressInspectIsUnavailableNotNotLocal(t *testing.T) {
	r := &issue115AppleAddressRunner{
		fixture:           issue115AppleImageFixture(t),
		localIdentityless: true,
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithPullPolicy(PullNever), withRunner(r), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityUnavailable) {
		t.Fatalf("error = %v, want ErrImageIdentityUnavailable", err)
	}
	if errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("successful identity-less inspect was misclassified as not-local: %v", err)
	}
}

func TestIssue115ApplePrefixedIDIsNotAnImmutableRunReference(t *testing.T) {
	prefixed := "sha256:" + issue115AppleImageID
	data := []byte(fmt.Sprintf(`[{"id":%q,"reference":%q,"descriptor":{"digest":%q}}]`, prefixed, prefixed, prefixed))
	identity, exists := (appleEngine{}).parseImageIdentity(data, prefixed, "")
	if !exists {
		t.Fatal("inspect should report that the image record exists")
	}
	if identity.pinned {
		t.Fatalf("Apple accepted Docker-style image ID as immutable: %+v", identity)
	}
}

func TestIssue115AppleUnprefixedIDRequiresMatchingDescriptor(t *testing.T) {
	digest := "sha256:" + issue115AppleImageID
	data := []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"name":"redis:7-alpine","descriptor":{"digest":%q}},"variants":[]}]`, issue115AppleImageID, digest))
	identity, exists := (appleEngine{}).parseImageIdentity(data, issue115AppleImageID, "")
	if !exists || !identity.pinned || identity.id != "" {
		t.Fatalf("identity = %+v, exists = %v, want descriptor-verified normalized ID", identity, exists)
	}
	if identity.reference != "redis:7-alpine@"+digest {
		t.Fatalf("reference = %q, want repository digest reference", identity.reference)
	}
}

func TestIssue115ApplePrefixedIDWithoutDescriptorFailsClosed(t *testing.T) {
	prefixed := "sha256:" + issue115AppleImageID
	data := []byte(fmt.Sprintf(`[{"id":%q,"configuration":{"name":"redis:7-alpine"},"variants":[]}]`, prefixed))
	identity, exists := (appleEngine{}).parseImageIdentity(data, "redis:7-alpine", "")
	if !exists {
		t.Fatal("inspect should report that the image record exists")
	}
	if identity.pinned {
		t.Fatalf("Apple treated an unverified prefixed ID as immutable: %+v", identity)
	}
}

func TestIssue115DockerImageIDRequiresMatchingVerifiedID(t *testing.T) {
	requested := "sha256:" + issue115AppleImageID
	data := []byte(fmt.Sprintf(`[{"Id":"sha256:%064x","RepoDigests":["docker.io/library/redis@sha256:%064x"]}]`, 4, 5))
	identity, exists := (dockerEngine{}).parseImageIdentity(data, requested, "")
	if !exists || identity.pinned {
		t.Fatalf("identity = %+v, exists = %v, want unverified ID mismatch", identity, exists)
	}

	matching := []byte(fmt.Sprintf(`[{"Id":%q}]`, requested))
	identity, exists = (dockerEngine{}).parseImageIdentity(matching, requested, "")
	if !exists || !identity.pinned || identity.id != requested {
		t.Fatalf("identity = %+v, exists = %v, want verified Docker ID", identity, exists)
	}
}

func TestIssue115BareDigestIdentityRequiresProvenanceOrVerifiedID(t *testing.T) {
	digest := "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	bare := imageIdentity{reference: digest, digest: digest, pinned: true}
	repository := imageIdentity{reference: "redis:7-alpine@" + digest, digest: digest, pinned: true}
	if imageIdentitiesCompatible(bare, repository) {
		t.Fatal("bare digest matched a repository identity without provenance")
	}
	if imagesCompatible(digest, repository.reference) {
		t.Fatal("bare digest compatibility accepted a repository-backed identity")
	}
	if imagesCompatible("sha256:not-a-valid-digest", "sha256:not-a-valid-digest") {
		t.Fatal("malformed bare digest compatibility must fail closed")
	}
	verifiedA := bare
	verifiedA.id = digest
	verifiedB := repository
	verifiedB.id = digest
	if !imageIdentitiesCompatible(verifiedA, verifiedB) {
		t.Fatal("matching verified image IDs should remain compatible")
	}
	oneSidedID := repository
	oneSidedID.id = digest
	if !imageIdentitiesCompatible(verifiedA, oneSidedID) {
		t.Fatal("repository provenance plus a verified ID should remain compatible")
	}
}

func policyName(policy PullPolicy) string {
	switch policy {
	case PullMissing:
		return "PullMissing"
	case PullAlways:
		return "PullAlways"
	case PullNever:
		return "PullNever"
	default:
		return "unknown"
	}
}
