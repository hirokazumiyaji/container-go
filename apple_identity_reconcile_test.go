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

func reconcileImageJSON(platform, variant string, synthetic bool) []byte {
	variants := []any{freshVariant(platform, variant)}
	annotations := map[string]any{}
	if synthetic {
		annotations["com.apple.containerization.index.indirect"] = "true"
	}
	data := map[string]any{
		"id": strings.TrimPrefix(finalRoot, "sha256:"),
		"configuration": map[string]any{
			"name": "registry.example/team/demo:stable",
			"descriptor": map[string]any{
				"digest":      finalRoot,
				"mediaType":   "application/vnd.oci.image.index.v1+json",
				"annotations": annotations,
			},
		},
		"variants": variants,
	}
	out, _ := json.Marshal([]any{data})
	return out
}

type reconcileRunner struct {
	mu sync.Mutex

	completeJSON  []byte
	postPlatform  string
	imageInspects int
	runImage      string
	runPlatform   string
	creation      string
	deleteCalls   int
}

func (r *reconcileRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		r.imageInspects++
		if r.imageInspects == 1 {
			return []byte(`[{"configuration":{"name":"registry.example/team/demo:stable"}}]`), nil, nil
		}
		return r.completeJSON, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			if arg == "--platform" && i+1 < len(args) {
				r.runPlatform = args[i+1]
			}
			if arg == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				r.creation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		return finalContainerJSON(r.runImage, finalRoot, finalVariant, r.postPlatform, r.creation), nil, nil
	case args[0] == "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestAppleExactInspectReconcilesCompleteIdentity(t *testing.T) {
	host := freshHostPlatform()
	runner := &reconcileRunner{
		completeJSON: reconcileImageJSON(host, finalVariant, true),
		postPlatform: host,
	}
	input := "registry.example/team/demo:stable@" + finalRoot
	ctr, err := Run(context.Background(), input,
		WithName("myctr"), WithPlatform(host), withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.imageInspects != 2 {
		t.Fatalf("image inspects = %d, want initial plus exact verification", runner.imageInspects)
	}
	if runner.runImage != input {
		t.Fatalf("run image = %q, want caller reference %q", runner.runImage, input)
	}
	if ctr.image.reference != input || ctr.image.digest != finalRoot || ctr.image.rootDigest != finalRoot {
		t.Fatalf("root/reference identity = %+v, want caller reference and verified root", ctr.image)
	}
	if ctr.image.repository != "registry.example/team/demo" || ctr.image.platform != host || ctr.image.variantDigest != finalVariant || !ctr.image.appleSynthetic {
		t.Fatalf("reconciled identity = %+v, want complete platform/variant/synthetic metadata", ctr.image)
	}
}

func TestAppleExactInspectStillChecksPostCreatePlatform(t *testing.T) {
	host := freshHostPlatform()
	runner := &reconcileRunner{
		completeJSON: reconcileImageJSON(host, finalVariant, true),
		postPlatform: freshOtherPlatform(host),
	}
	input := "registry.example/team/demo:stable@" + finalRoot
	_, err := Run(context.Background(), input,
		WithName("myctr"), WithPlatform(host), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want post-create platform mismatch", err)
	}
	if runner.deleteCalls != 1 || runner.runImage == "" {
		t.Fatalf("platform mismatch was not rolled back: run=%q deletes=%d", runner.runImage, runner.deleteCalls)
	}
}

type scopedIndexRunner struct {
	mu sync.Mutex

	imageJSON        []byte
	exactAddressable bool
	pullTargets      []string
	runImage         string
	runPlatform      string
	creation         string
}

func (r *scopedIndexRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
		if strings.Contains(args[len(args)-1], "@") && !r.exactAddressable {
			return nil, nil, &cli.CLIError{Binary: "container", Args: args, ExitCode: 1, Stderr: "image not found: " + args[len(args)-1]}
		}
		return r.imageJSON, nil, nil
	case len(args) >= 2 && args[0] == "image" && args[1] == "pull":
		r.pullTargets = append(r.pullTargets, args[len(args)-1])
		r.exactAddressable = true
		return nil, nil, nil
	case args[0] == "run":
		r.runImage = args[len(args)-1]
		for i, arg := range args {
			if arg == "--platform" && i+1 < len(args) {
				r.runPlatform = args[i+1]
			}
			if arg == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], creationLabel+"=") {
				r.creation = strings.TrimPrefix(args[i+1], creationLabel+"=")
			}
		}
		return []byte("myctr\n"), nil, nil
	case args[0] == "inspect":
		return finalContainerJSON(r.runImage, finalRoot, finalVariant, r.runPlatform, r.creation), nil, nil
	default:
		return nil, nil, nil
	}
}

func ordinaryScopedIndexJSON(platform string) []byte {
	return reconcileImageJSON(platform, finalVariant, false)
}

func TestAppleOrdinaryOneVariantIndexUsesControlledExactRootProbe(t *testing.T) {
	host := freshHostPlatform()
	runner := &scopedIndexRunner{imageJSON: ordinaryScopedIndexJSON(host)}
	ctr, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullMissing),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != finalRoot || ctr.image.platform != host || ctr.image.variantDigest != finalVariant {
		t.Fatalf("identity = %+v, want ordinary scoped index identity", ctr.image)
	}
	if len(runner.pullTargets) != 1 || !strings.Contains(runner.pullTargets[0], "@"+finalRoot) {
		t.Fatalf("pull targets = %v, want one controlled exact-root probe", runner.pullTargets)
	}
	if runner.runImage != "registry.example/team/demo:stable@"+finalRoot || runner.runPlatform != host {
		t.Fatalf("run = %q platform=%q, want pinned scoped variant", runner.runImage, runner.runPlatform)
	}
}

func TestAppleOrdinaryOneVariantIndexWorksForPullNever(t *testing.T) {
	host := freshHostPlatform()
	runner := &scopedIndexRunner{
		imageJSON:        ordinaryScopedIndexJSON(host),
		exactAddressable: true,
	}
	ctr, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(applePullNeverBelowPreflight{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != finalRoot || ctr.image.platform != host || len(runner.pullTargets) != 0 {
		t.Fatalf("identity = %+v pulls=%v, want local ordinary scoped index", ctr.image, runner.pullTargets)
	}
}

func TestAppleSyntheticRequiresReliableSignal(t *testing.T) {
	host := freshHostPlatform()
	ordinary, exists := (appleEngine{}).parseImageIdentity(ordinaryScopedIndexJSON(host), "registry.example/team/demo:stable", host)
	if !exists || ordinary.appleSynthetic {
		t.Fatalf("ordinary one-variant identity = %+v, exists=%v, want non-synthetic", ordinary, exists)
	}
	synthetic, exists := (appleEngine{}).parseImageIdentity(reconcileImageJSON(host, finalVariant, true), "registry.example/team/demo:stable", host)
	if !exists || !synthetic.appleSynthetic {
		t.Fatalf("annotated synthetic identity = %+v, exists=%v, want synthetic", synthetic, exists)
	}
}

func TestAppleOrdinaryOneVariantPullNeverRejectsMissingRootWithoutLabelingSynthetic(t *testing.T) {
	host := freshHostPlatform()
	runner := &scopedIndexRunner{imageJSON: ordinaryScopedIndexJSON(host)}
	_, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(applePullNeverBelowPreflight{}))
	if !errors.Is(err, ErrImageIdentityNotLocal) {
		t.Fatalf("error = %v, want ErrImageIdentityNotLocal", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "synthetic") {
		t.Fatalf("ordinary index was mislabeled synthetic: %v", err)
	}
	if len(runner.pullTargets) != 0 || runner.runImage != "" {
		t.Fatalf("PullNever fetched or ran a missing root: pulls=%v run=%q", runner.pullTargets, runner.runImage)
	}
}

func TestReconcileFixtureHasSyntheticAnnotation(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(finalFixture(t), "team/demo:stable", "")
	if !exists || !identity.appleSynthetic {
		t.Fatalf("single-manifest fixture identity = %+v, exists=%v, want annotated synthetic index", identity, exists)
	}
}
