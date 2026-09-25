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

func issue115ReconcileImageJSON(platform, variant string, synthetic bool) []byte {
	variants := []any{issue115FreshVariant(platform, variant)}
	annotations := map[string]any{}
	if synthetic {
		annotations["com.apple.containerization.index.indirect"] = "true"
	}
	data := map[string]any{
		"id": strings.TrimPrefix(issue115FinalRoot, "sha256:"),
		"configuration": map[string]any{
			"name": "registry.example/team/demo:stable",
			"descriptor": map[string]any{
				"digest":      issue115FinalRoot,
				"mediaType":   "application/vnd.oci.image.index.v1+json",
				"annotations": annotations,
			},
		},
		"variants": variants,
	}
	out, _ := json.Marshal([]any{data})
	return out
}

type issue115ReconcileRunner struct {
	mu sync.Mutex

	completeJSON  []byte
	postPlatform  string
	imageInspects int
	runImage      string
	runPlatform   string
	creation      string
	deleteCalls   int
}

func (r *issue115ReconcileRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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
		return issue115FinalContainerJSON(r.runImage, issue115FinalRoot, issue115FinalVariant, r.postPlatform, r.creation), nil, nil
	case args[0] == "delete":
		r.deleteCalls++
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

func TestIssue115AppleExactInspectReconcilesCompleteIdentity(t *testing.T) {
	host := issue115FreshHostPlatform()
	runner := &issue115ReconcileRunner{
		completeJSON: issue115ReconcileImageJSON(host, issue115FinalVariant, true),
		postPlatform: host,
	}
	input := "registry.example/team/demo:stable@" + issue115FinalRoot
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
	if ctr.image.reference != input || ctr.image.digest != issue115FinalRoot || ctr.image.rootDigest != issue115FinalRoot {
		t.Fatalf("root/reference identity = %+v, want caller reference and verified root", ctr.image)
	}
	if ctr.image.repository != "registry.example/team/demo" || ctr.image.platform != host || ctr.image.variantDigest != issue115FinalVariant || !ctr.image.appleSynthetic {
		t.Fatalf("reconciled identity = %+v, want complete platform/variant/synthetic metadata", ctr.image)
	}
}

func TestIssue115AppleExactInspectStillChecksPostCreatePlatform(t *testing.T) {
	host := issue115FreshHostPlatform()
	runner := &issue115ReconcileRunner{
		completeJSON: issue115ReconcileImageJSON(host, issue115FinalVariant, true),
		postPlatform: issue115FreshOtherPlatform(host),
	}
	input := "registry.example/team/demo:stable@" + issue115FinalRoot
	_, err := Run(context.Background(), input,
		WithName("myctr"), WithPlatform(host), withRunner(runner), withEngine(appleEngine{}))
	if !errors.Is(err, ErrImageIdentityMismatch) {
		t.Fatalf("error = %v, want post-create platform mismatch", err)
	}
	if runner.deleteCalls != 1 || runner.runImage == "" {
		t.Fatalf("platform mismatch was not rolled back: run=%q deletes=%d", runner.runImage, runner.deleteCalls)
	}
}

type issue115ScopedIndexRunner struct {
	mu sync.Mutex

	imageJSON        []byte
	exactAddressable bool
	pullTargets      []string
	runImage         string
	runPlatform      string
	creation         string
}

func (r *issue115ScopedIndexRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
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
		return issue115FinalContainerJSON(r.runImage, issue115FinalRoot, issue115FinalVariant, r.runPlatform, r.creation), nil, nil
	default:
		return nil, nil, nil
	}
}

func issue115OrdinaryScopedIndexJSON(platform string) []byte {
	return issue115ReconcileImageJSON(platform, issue115FinalVariant, false)
}

func TestIssue115AppleOrdinaryOneVariantIndexUsesControlledExactRootProbe(t *testing.T) {
	host := issue115FreshHostPlatform()
	runner := &issue115ScopedIndexRunner{imageJSON: issue115OrdinaryScopedIndexJSON(host)}
	ctr, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullMissing),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != issue115FinalRoot || ctr.image.platform != host || ctr.image.variantDigest != issue115FinalVariant {
		t.Fatalf("identity = %+v, want ordinary scoped index identity", ctr.image)
	}
	if len(runner.pullTargets) != 1 || !strings.Contains(runner.pullTargets[0], "@"+issue115FinalRoot) {
		t.Fatalf("pull targets = %v, want one controlled exact-root probe", runner.pullTargets)
	}
	if runner.runImage != "registry.example/team/demo:stable@"+issue115FinalRoot || runner.runPlatform != host {
		t.Fatalf("run = %q platform=%q, want pinned scoped variant", runner.runImage, runner.runPlatform)
	}
}

func TestIssue115AppleOrdinaryOneVariantIndexWorksForPullNever(t *testing.T) {
	host := issue115FreshHostPlatform()
	runner := &issue115ScopedIndexRunner{
		imageJSON:        issue115OrdinaryScopedIndexJSON(host),
		exactAddressable: true,
	}
	ctr, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(appleEngine{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctr.image.digest != issue115FinalRoot || ctr.image.platform != host || len(runner.pullTargets) != 0 {
		t.Fatalf("identity = %+v pulls=%v, want local ordinary scoped index", ctr.image, runner.pullTargets)
	}
}

func TestIssue115AppleSyntheticRequiresReliableSignal(t *testing.T) {
	host := issue115FreshHostPlatform()
	ordinary, exists := (appleEngine{}).parseImageIdentity(issue115OrdinaryScopedIndexJSON(host), "registry.example/team/demo:stable", host)
	if !exists || ordinary.appleSynthetic {
		t.Fatalf("ordinary one-variant identity = %+v, exists=%v, want non-synthetic", ordinary, exists)
	}
	synthetic, exists := (appleEngine{}).parseImageIdentity(issue115ReconcileImageJSON(host, issue115FinalVariant, true), "registry.example/team/demo:stable", host)
	if !exists || !synthetic.appleSynthetic {
		t.Fatalf("annotated synthetic identity = %+v, exists=%v, want synthetic", synthetic, exists)
	}
}

func TestIssue115AppleOrdinaryOneVariantPullNeverRejectsMissingRootWithoutLabelingSynthetic(t *testing.T) {
	host := issue115FreshHostPlatform()
	runner := &issue115ScopedIndexRunner{imageJSON: issue115OrdinaryScopedIndexJSON(host)}
	_, err := Run(context.Background(), "registry.example/team/demo:stable",
		WithName("myctr"), WithPlatform(host), WithPullPolicy(PullNever),
		withRunner(runner), withEngine(appleEngine{}))
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

func TestIssue115ReconcileFixtureHasSyntheticAnnotation(t *testing.T) {
	identity, exists := (appleEngine{}).parseImageIdentity(issue115FinalFixture(t), "team/demo:stable", "")
	if !exists || !identity.appleSynthetic {
		t.Fatalf("single-manifest fixture identity = %+v, exists=%v, want annotated synthetic index", identity, exists)
	}
}
