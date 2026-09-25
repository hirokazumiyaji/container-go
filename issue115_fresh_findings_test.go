package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

func TestIssue115AppleManifestUnknownAllowsExplicitMutableFallback(t *testing.T) {
	exact := "redis:7-alpine@" + issue115ReviewRoot
	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "manifest unknown code",
			stderr: `Error: registry returned {"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`,
		},
		{
			name:   "documented http 404",
			stderr: `Error: unknown: "HTTP request to https://registry.example/v2/manifests/latest failed with response: 404 Not Found. Reason: Unknown"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pullErr := &cli.CLIError{
				Binary:   "container",
				Args:     []string{"image", "pull", exact},
				ExitCode: 1,
				Stderr:   tc.stderr,
			}
			if !(appleEngine{}).imageMissing(pullErr) || !imageAddressMissing(appleEngine{}, pullErr) {
				t.Fatalf("direct Apple pull classifier rejected %q", tc.stderr)
			}
			r := &issue115AppleAddressRunner{
				fixture:      issue115AppleImageFixture(t),
				localMissing: true,
				exactPullErr: pullErr,
			}
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithAllowMutableImageTag(), withRunner(r), withEngine(appleEngine{}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctr.image.pinned {
				t.Fatal("manifest-unknown fallback was marked pinned")
			}
			runImage, _, pullTargets := r.state()
			if runImage != "redis:7-alpine" {
				t.Fatalf("run image = %q, want caller mutable reference", runImage)
			}
			if len(pullTargets) != 1 || pullTargets[0] != exact {
				t.Fatalf("pull targets = %v, want one exact-root attempt", pullTargets)
			}
		})
	}
}

func TestIssue115AppleManifestUnknownIsPullOnlyAndNotTransport(t *testing.T) {
	pullErr := func(stderr string) error {
		return &cli.CLIError{
			Binary:   "container",
			Args:     []string{"image", "pull", "redis:7-alpine"},
			ExitCode: 1,
			Stderr:   stderr,
		}
	}
	if !(appleEngine{}).imageMissing(pullErr(`MANIFEST_UNKNOWN`)) {
		t.Fatal("Apple image-pull MANIFEST_UNKNOWN was not classified as absent")
	}
	if !(appleEngine{}).imageMissing(pullErr(`HTTP request failed with response: 404 Not Found`)) {
		t.Fatal("Apple image-pull HTTP 404 was not classified as absent")
	}
	for _, stderr := range []string{`MANIFEST_UNKNOWN`, `HTTP response: 404 Not Found`} {
		if (appleEngine{}).imageMissing(&cli.CLIError{
			Binary:   "container",
			Args:     []string{"image", "inspect", "redis:7-alpine"},
			ExitCode: 1,
			Stderr:   stderr,
		}) {
			t.Fatalf("Apple image inspect pull-only reason was classified as absent: %q", stderr)
		}
	}
	for _, stderr := range []string{
		`permission denied: MANIFEST_UNKNOWN`,
		`permission denied: HTTP response: 404 Not Found`,
		`connection refused: HTTP response: 404 Not Found`,
		`XPC connection error: 404`,
	} {
		if (appleEngine{}).imageMissing(pullErr(stderr)) {
			t.Fatalf("transport/permission error was classified as absent: %q", stderr)
		}
	}
}

type issue115DockerImageAbsenceRunner struct {
	*fakeRunner
	reason    string
	available bool
	pullCalls int
}

func (r *issue115DockerImageAbsenceRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		if r.available {
			r.imagePresent = true
			return r.fakeRunner.Run(ctx, args...)
		}
		return nil, nil, &cli.CLIError{
			Binary:   "docker",
			Args:     append([]string(nil), args...),
			ExitCode: 1,
			Stderr:   r.reason,
		}
	}
	if len(args) > 0 && (args[0] == "pull" || (args[0] == "image" && len(args) > 1 && args[1] == "pull")) {
		r.available = true
		r.pullCalls++
		r.imagePresent = true
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestIssue115DockerPlatformAndManifestAbsenceDrivePullPolicy(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{
			name:   "platform mismatch",
			reason: "Error response from daemon: no matching manifest for linux/arm64/v8 in the manifest list entries",
		},
		{
			name:   "manifest unknown",
			reason: "Error response from daemon: manifest unknown: docker.io/library/redis:7-alpine",
		},
		{
			name:   "manifest unknown code",
			reason: `Error response from daemon: {"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []PullPolicy{PullMissing, PullNever} {
				t.Run(policyName(policy), func(t *testing.T) {
					r := &issue115DockerImageAbsenceRunner{
						fakeRunner: newTestRunner(),
						reason:     tc.reason,
					}
					_, err := Run(context.Background(), "docker.io/library/redis:7-alpine",
						WithName("myctr"), WithPlatform("linux/arm64/v8"), WithPullPolicy(policy),
						withRunner(r), withEngine(dockerEngine{}))
					if policy == PullNever {
						if !errors.Is(err, ErrImageNotFound) {
							t.Fatalf("error = %v, want ErrImageNotFound", err)
						}
						if r.pullCalls != 0 || r.callWith("run") != nil {
							t.Fatalf("PullNever fetched or ran: pulls=%d calls=%v", r.pullCalls, r.calls)
						}
					} else {
						if err != nil {
							t.Fatalf("PullMissing Run: %v", err)
						}
						if r.pullCalls != 1 || r.callWith("run") == nil {
							t.Fatalf("PullMissing did not pull and run once: pulls=%d calls=%v", r.pullCalls, r.calls)
						}
					}
				})
			}
		})
	}
}

func TestIssue115DockerAbsenceMatcherIsOperationScoped(t *testing.T) {
	reason := "no matching manifest for linux/arm64/v8 in the manifest list entries"
	if (dockerEngine{}).imageMissing(&cli.CLIError{
		Binary:   "docker",
		Args:     []string{"run", "--platform", "linux/arm64/v8", "redis"},
		ExitCode: 1,
		Stderr:   reason,
	}) {
		t.Fatal("run-time platform mismatch was classified as image absence")
	}
	if (dockerEngine{}).imageMissing(&cli.CLIError{
		Binary:   "docker",
		Args:     []string{"image", "inspect", "redis"},
		ExitCode: 1,
		Stderr:   "pull access denied: " + reason,
	}) {
		t.Fatal("permission error was classified as image absence")
	}
}

func TestIssue115DockerHubAliasesAreCanonicalForRepositoryComparisons(t *testing.T) {
	cases := []struct {
		requested string
		actual    string
	}{
		{
			requested: "docker.io/library/redis:7-alpine",
			actual:    "registry-1.docker.io/library/redis:7-alpine",
		},
		{
			requested: "index.docker.io/library/redis:7-alpine",
			actual:    "docker.io/redis:7-alpine",
		},
		{
			requested: "registry-1.docker.io/redis",
			actual:    "redis",
		},
	}
	for _, tc := range cases {
		if !imagesCompatible(tc.requested, tc.actual) {
			t.Errorf("imagesCompatible(%q, %q) = false", tc.requested, tc.actual)
		}
		if !imageRepositoryPathsCompatible(tc.requested, tc.actual) {
			t.Errorf("imageRepositoryPathsCompatible(%q, %q) = false", tc.requested, tc.actual)
		}
	}
	if got := imageRepository("registry-1.docker.io/redis"); got != "docker.io/library/redis" {
		t.Fatalf("imageRepository(alias) = %q, want canonical implicit-library path", got)
	}
	if imageRepositoriesCompatible("docker.io/library/redis", "registry.example/team/redis") {
		t.Fatal("custom registry was treated as a Docker Hub alias")
	}

	root := "sha256:" + strings.Repeat("a", 64)
	first := imageIdentity{
		reference:  "docker.io/library/redis@" + root,
		digest:     root,
		rootDigest: root,
		repository: "docker.io/library/redis",
		pinned:     true,
	}
	second := first
	second.reference = "registry-1.docker.io/library/redis@" + root
	second.repository = "registry-1.docker.io/library/redis"
	if !imageIdentitiesCompatible(first, second) {
		t.Fatal("Docker Hub aliases were not canonical in immutable identity comparison")
	}
}

func TestIssue115LifecycleClassifiersAreOperationBackendAndTargetAware(t *testing.T) {
	appleConflict := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"run", "--name", "wanted", "redis"},
		ExitCode: 1,
		Stderr:   `Error: container "wanted" already exists`,
	}
	if !(appleEngine{}).nameConflict(lifecycleRun, "wanted", appleConflict) {
		t.Fatal("Apple name conflict was not recognized")
	}
	if (appleEngine{}).nameConflict(lifecycleInspect, "wanted", appleConflict) {
		t.Fatal("Apple name conflict was accepted for inspect")
	}
	if (appleEngine{}).nameConflict(lifecycleRun, "other", appleConflict) {
		t.Fatal("Apple name conflict was accepted for a different target")
	}
	if (appleEngine{}).nameConflict(lifecycleRun, "wanted", &cli.CLIError{
		Binary:   "docker",
		Args:     appleConflict.Args,
		ExitCode: 1,
		Stderr:   appleConflict.Stderr,
	}) {
		t.Fatal("Apple name conflict accepted a Docker error")
	}
	for _, stderr := range []string{
		`Error: image configuration "wanted" already exists`,
		`Error: container configuration "wanted" already exists`,
	} {
		if (appleEngine{}).nameConflict(lifecycleRun, "wanted", &cli.CLIError{
			Binary:   "container",
			Args:     appleConflict.Args,
			ExitCode: 1,
			Stderr:   stderr,
		}) {
			t.Fatalf("image/config conflict was classified as a container name conflict: %q", stderr)
		}
	}

	dockerConflict := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"run", "--name", "wanted", "redis"},
		ExitCode: 1,
		Stderr:   `Conflict. The container name "/wanted" is already in use by container`,
	}
	if !(dockerEngine{}).nameConflict(lifecycleRun, "wanted", dockerConflict) {
		t.Fatal("Docker name conflict was not recognized")
	}
	if (dockerEngine{}).nameConflict(lifecycleRun, "wanted", &cli.CLIError{
		Binary:   "docker",
		Args:     dockerConflict.Args,
		ExitCode: 1,
		Stderr:   `Conflict. The image configuration "wanted" already exists`,
	}) {
		t.Fatal("Docker image configuration conflict was classified as a container name conflict")
	}

	missing := &cli.CLIError{
		Binary:   "docker",
		Args:     []string{"inspect", "wanted"},
		ExitCode: 1,
		Stderr:   `Error: No such object: wanted`,
	}
	if !(dockerEngine{}).containerMissing(lifecycleInspect, "wanted", missing) {
		t.Fatal("Docker missing container was not recognized")
	}
	if (dockerEngine{}).containerMissing(lifecycleRun, "wanted", missing) {
		t.Fatal("Docker missing container was accepted for run")
	}
	if (dockerEngine{}).containerMissing(lifecycleInspect, "other", missing) {
		t.Fatal("Docker missing container was accepted for another target")
	}
	for _, stderr := range []string{
		`/bin/sh: missing-command: not found`,
		`application error: not found: "wanted"`,
	} {
		if (dockerEngine{}).containerMissing(lifecycleExec, "wanted", &cli.CLIError{
			Binary:   "docker",
			Args:     []string{"exec", "wanted", "missing-command"},
			ExitCode: 127,
			Stderr:   stderr,
		}) {
			t.Fatalf("application not-found was classified as a missing container: %q", stderr)
		}
	}

	race := &cli.CLIError{
		Binary:   "container",
		Args:     []string{"run", "--name", "wanted", "redis"},
		ExitCode: 1,
		Stderr:   `Error: container with id "wanted" not found`,
	}
	if !(appleEngine{}).createRaceMissing(lifecycleRun, "wanted", race) {
		t.Fatal("Apple create race was not recognized")
	}
	if (appleEngine{}).createRaceMissing(lifecycleInspect, "wanted", race) {
		t.Fatal("Apple create race was accepted for inspect")
	}
	if (dockerEngine{}).createRaceMissing(lifecycleRun, "wanted", race) {
		t.Fatal("Docker was classified as having Apple's create race")
	}
}

func TestIssue115RunDoesNotSkipCleanupForImageConfigConflict(t *testing.T) {
	r := &failRunRunner{
		fakeRunner: newTestRunner(),
		runErr: &cli.CLIError{
			Binary:   "container",
			Args:     []string{"run", "--name", "myctr", "redis"},
			ExitCode: 1,
			Stderr:   `Error: image configuration "myctr" already exists`,
		},
		inspectJSON: ownedInspectJSON("myctr"),
	}
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), withRunner(r), withEngine(appleEngine{}))
	if err == nil {
		t.Fatal("Run unexpectedly succeeded after image-config conflict")
	}
	if len(r.deleted) != 1 {
		t.Fatalf("cleanup deletes = %d, want owned container cleanup", len(r.deleted))
	}
}

type issue115ExecAppRunner struct {
	*fakeRunner
	inspectCalls int
}

func (r *issue115ExecAppRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "exec" {
		return nil, []byte("missing-command: not found\n"), &cli.CLIError{
			Binary:   "docker",
			Args:     append([]string(nil), args...),
			ExitCode: 127,
			Stderr:   "/bin/sh: missing-command: not found",
		}
	}
	if len(args) > 0 && args[0] == "inspect" {
		r.inspectCalls++
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestIssue115ExecApplicationNotFoundDoesNotProbeAsMissingContainer(t *testing.T) {
	base := newTestRunner()
	ctr := runTestContainer(t, base, withEngine(dockerEngine{}))
	r := &issue115ExecAppRunner{fakeRunner: base}
	ctr.runner = r
	code, _, err := ctr.Exec(context.Background(), []string{"missing-command"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 127 {
		t.Fatalf("exit code = %d, want 127", code)
	}
	if r.inspectCalls != 0 {
		t.Fatalf("application not-found triggered %d inspect probes", r.inspectCalls)
	}
}
